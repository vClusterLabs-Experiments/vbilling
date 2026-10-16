#!/usr/bin/env python3
"""Validate a captured join command and save a private VM startup script.

This does not create a token or VM. Capture `vcluster token create --expires 1h`
output to a 0600 file outside Git, then pass only its path here. Never use set -x.
"""
import argparse
import ipaddress
import os
import re
import sys
import urllib.parse
from demo_common import GuardError, outside_git, private_read, require

def render(raw, allowed_host):
    lines=[line.strip() for line in raw.splitlines() if line.strip().startswith('curl ')]
    require(len(lines)==1,'Expected exactly one join command')
    match=re.fullmatch(r'curl -fsSLk "(https://[^"\s]+/node/join\?token=[A-Za-z0-9.%_-]+)" \| sh -',lines[0])
    require(match is not None,'Unexpected join command; raw output suppressed')
    url=urllib.parse.urlsplit(match.group(1))
    allowed_hosts=[allowed_host] if isinstance(allowed_host,str) else allowed_host
    require(url.hostname in allowed_hosts and not url.username and not url.password and not url.fragment,
            'Join endpoint differs from the explicitly allowed host')
    return '''#!/bin/bash
set +x
set -euo pipefail
umask 077
exec >> /var/log/vcluster-join.log 2>&1
[ -f /var/lib/vcluster-joined ] && exit 0
'''+lines[0]+'''
touch /var/lib/vcluster-joined
echo PRIVATE_NODE_JOIN_COMPLETE
'''

def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--join-file',required=True)
    p.add_argument('--allowed-host',required=True,action='append',help='Exact host/IP observed in the private endpoint or Platform URL')
    p.add_argument('--output',required=True)
    a=p.parse_args()
    out=outside_git(a.output); require(not out.exists(),'Output exists; do not overwrite token material')
    result=render(private_read(a.join_file),a.allowed_host)
    fd=os.open(out,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'w') as f: f.write(result)
    print('Private startup file prepared; remove it and VM startup metadata after the node joins.')
if __name__=='__main__':
    try: main()
    except GuardError as e: print('Stopped: '+str(e),file=sys.stderr); sys.exit(2)
    except (OSError,ValueError): print('Startup preparation failed; raw credentials suppressed.',file=sys.stderr); sys.exit(2)
