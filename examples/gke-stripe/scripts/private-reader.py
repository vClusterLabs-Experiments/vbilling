#!/usr/bin/env python3
"""Export only the named read-only tenant Secret into private local files."""
import argparse
import base64
import json
import os
from pathlib import Path
import sys
import tempfile
from demo_common import GuardError, command, outside_git, private_read, require, run_id, save_json

def main():
    p=argparse.ArgumentParser(description=__doc__)
    for key in ('host-kubeconfig','namespace','name','tenant','region','run-dir'): p.add_argument('--'+key, required=True)
    a=p.parse_args()
    root=outside_git(a.run_dir)
    require(a.tenant.endswith('-private'), 'Use the run ID-private tenant')
    run_id(a.tenant[:-8]); require(a.namespace == a.tenant, 'Private namespace must match private tenant')
    require(a.name == 'private-a100', 'This demo exports the private-a100 reader only')
    private_read(a.host_kubeconfig)
    out=root/'private-reader.kubeconfig'; tenant_file=root/'private-tenants.json'
    require(root.is_dir() and not out.exists() and not tenant_file.exists(), 'Private output must be new; preserve existing reader files')
    secret=json.loads(command(['kubectl','--kubeconfig',a.host_kubeconfig,'-n',a.namespace,'get','secret','vbilling-kubeconfig','-o','json']))
    raw=next((secret.get('data',{}).get(k) for k in ('config','kubeconfig','value') if secret.get('data',{}).get(k)), None)
    require(bool(raw), 'Reader Secret has no kubeconfig')
    fd,temp=tempfile.mkstemp(prefix='.reader-',dir=root)
    try:
        with os.fdopen(fd,'wb') as f: f.write(base64.b64decode(raw,validate=True))
        cfg=json.loads(command(['kubectl','--kubeconfig',temp,'config','view','--raw','-o','json']))
    finally: os.unlink(temp)
    contexts={c['name']:c['context'] for c in cfg['contexts']}
    target=contexts[cfg['current-context']]['cluster']
    matched=False
    for row in cfg['clusters']:
        if row['name']==target:
            row['cluster']['server']='https://127.0.0.1:18446'
            row['cluster']['tls-server-name']=a.name+'.'+a.namespace
            row['cluster'].pop('insecure-skip-tls-verify',None)
            require(bool(row['cluster'].get('certificate-authority-data')), 'Reader needs a trusted CA')
            matched=True
    require(matched, 'Reader cluster is missing')
    save_json(out,cfg,exclusive=True)
    save_json(tenant_file,[{'name':a.name,'kubeconfig':str(out),'tenant':a.tenant,
        'displayName':'Private A100 example','metadata':{'plan':a.tenant,'region':a.region}}],exclusive=True)
    print(json.dumps({'reader_file':str(out),'tenant_file':str(tenant_file),'server':'https://127.0.0.1:18446','credentials_printed':False}))

if __name__=='__main__':
    try: main()
    except GuardError as e: print('Stopped: '+str(e),file=sys.stderr); sys.exit(2)
    except (OSError, ValueError, KeyError): print('Reader export failed; raw credentials suppressed.',file=sys.stderr); sys.exit(2)
