#!/usr/bin/env python3
"""Read the local demo API without putting its token in shell history or argv."""
import argparse
import json
import sys
import urllib.error
import urllib.request
from demo_common import GuardError, private_read, require
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise GuardError('Redirect refused')
def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--token-file', required=True)
    p.add_argument('--path', default='/api/v1/status')
    a=p.parse_args()
    require(a.path.startswith('/api/v1/') and not any(s in a.path for s in (':','..','\r','\n','#')), 'Only a local vBilling API path is supported')
    token=private_read(a.token_file)
    req=urllib.request.Request('http://127.0.0.1:18090'+a.path, headers={'Authorization':'Bearer '+token})
    with urllib.request.build_opener(NoRedirect).open(req, timeout=30) as response:
        print(json.dumps(json.load(response), indent=2))
if __name__=='__main__':
    try: main()
    except (GuardError, OSError, ValueError, urllib.error.URLError):
        print('Local API read failed; raw response and token suppressed.', file=sys.stderr); sys.exit(2)
