#!/usr/bin/env python3
"""Exact-resource cleanup for a dedicated GPU example; no wildcard deletions.

Capture first records immutable resource IDs. Plan is offline. Execute rechecks
ownership and IDs, requires the exact run confirmation, and refuses cluster
deletion while Kubernetes PVs or LoadBalancer Services remain. It never deletes
unrecorded firewalls, disks, IPs, or other resources to force network deletion.
"""
import argparse
import json
import re
import subprocess
import sys
from demo_common import GuardError, outside_git, private_read, require, run_id, save_json

class GCloud:
    def __init__(self, project): self.project=project
    def run(self, args, absent_ok=False):
        proc=subprocess.run(['gcloud',*args,'--project='+self.project,'--quiet','--format=json'],capture_output=True,text=True)
        if proc.returncode:
            # IAM describe can report an ambiguous permission error for a deleted
            # service account. Never interpret that error itself as absence: a
            # separate successful, exact-email list must independently confirm it.
            denied = re.search(r'\bPERMISSION_DENIED\b', proc.stderr)
            if absent_ok and denied and len(args) == 4 and args[:3] == ['iam','service-accounts','describe'] and 'or it may not exist' in proc.stderr:
                email = args[3]
                require(re.fullmatch(r'[a-z][a-z0-9-]{4,29}@' + re.escape(self.project) + r'\.iam\.gserviceaccount\.com', email),
                        'Unexpected service account identity in absence check')
                rows = self.run(['iam','service-accounts','list','--filter=email=' + email])
                require(isinstance(rows, list) and all(isinstance(row, dict) and isinstance(row.get('email'), str) for row in rows),
                        'Service account list did not return a valid inventory')
                if not any(row['email'] == email for row in rows): return None
            # Other permission/connection errors remain failures even if their
            # diagnostic text happens to include words such as "not found".
            if absent_ok and not denied and re.search(r'\bNOT_FOUND\b|\bNotFound\b|was not found|could not be found|\bcode=404,\s*message=Not found:',proc.stderr): return None
            raise GuardError('GCP command failed: '+ ' '.join(args[:4]) + '. Cleanup stopped; earlier successful deletions may already have completed. Inspect this exact resource.')
        return json.loads(proc.stdout) if proc.stdout.strip() else {}

def specs(state):
    run=state['run']; zone=state['zone']; region=state['region']; project=state['project']
    rows={
        'cluster': ['container','clusters','describe',run,'--zone='+zone],
        'network': ['compute','networks','describe',run+'-net'],
        'subnet': ['compute','networks','subnets','describe',run+'-subnet','--region='+region],
        'service_account': ['iam','service-accounts','describe',run+'-node@'+project+'.iam.gserviceaccount.com'],
    }
    if state.get('private_zone'): rows['private_vm']=['compute','instances','describe',run+'-a100','--zone='+state['private_zone']]
    return rows

def identity(kind, value, state):
    run=state['run']; marker='vbilling-demo:'+run
    if kind=='cluster':
        require(value.get('name')==run and value.get('location')==state['zone'], 'Cluster identity mismatch')
        labels=value.get('resourceLabels') or {}
        require(labels.get('run')==run and labels.get('purpose')=='vbilling-demo', 'Cluster ownership labels differ')
        require(value.get('network','').split('/')[-1]==run+'-net' and value.get('subnetwork','').split('/')[-1]==run+'-subnet', 'Cluster network differs')
    elif kind in ('network','subnet'):
        suffix='-net' if kind=='network' else '-subnet'
        require(value.get('name')==run+suffix and value.get('description')==marker, 'Network/subnet ownership differs')
        if kind=='subnet': require(value.get('network','').endswith('/networks/'+run+'-net'),'Subnet network differs')
    elif kind=='service_account':
        require(value.get('email')==run+'-node@'+state['project']+'.iam.gserviceaccount.com' and
                value.get('displayName')=='vBilling demo '+run, 'Service account identity differs')
    else:
        labels=value.get('labels') or {}
        require(value.get('name')==run+'-a100' and labels.get('run')==run and labels.get('purpose')=='vbilling-demo', 'Private VM ownership differs')
        require(value.get('zone','').endswith('/zones/'+state['private_zone']), 'Private VM zone differs')
        disks=value.get('disks') or []
        require(len(disks)==1 and disks[0].get('boot') and disks[0].get('autoDelete'), 'Private VM must have only its auto-delete boot disk')
        interfaces=value.get('networkInterfaces') or []
        require(len(interfaces)==1 and interfaces[0].get('network','').endswith('/networks/'+run+'-net'), 'Private VM network differs')
    immutable=value.get('uniqueId') if kind=='service_account' else value.get('id')
    require(bool(immutable), 'Missing immutable provider ID for '+kind)
    return str(immutable)

def capture(args):
    run_id(args.run)
    require(re.fullmatch(r'[a-z][a-z0-9-]{4,61}[a-z0-9]',args.project), 'Invalid GCP project ID')
    for location in (args.zone,args.region,args.private_zone):
        if location: require(re.fullmatch(r'[a-z0-9-]+',location),'Invalid location')
    state={k:getattr(args,k) for k in ('project','run','zone','region','private_zone')}
    client=GCloud(state['project']); state['ids']={}
    for kind,argv in specs(state).items():
        value=client.run(argv)
        state['ids'][kind]=identity(kind,value,state)
    save_json(args.state,state,exclusive=True)
    print(json.dumps({'captured':state,'note':'Read-only capture. No deletion scheduled.'},indent=2))

def load(path):
    state=json.loads(private_read(path)); run_id(state['run'])
    require(set(state['ids'])==set(specs(state)), 'Cleanup manifest resources are incomplete or unexpected')
    return state

def kread(kubeconfig,args):
    proc=subprocess.run(['kubectl','--kubeconfig',kubeconfig,'--request-timeout=30s',*args],capture_output=True,text=True)
    require(proc.returncode==0,'Kubernetes preflight failed; cluster was not deleted')
    return json.loads(proc.stdout)

def ensure_drained(cluster, kubeconfig):
    private_read(kubeconfig)
    cfg=kread(kubeconfig,['config','view','--minify','-o','json'])
    endpoint=cfg['clusters'][0]['cluster']['server']
    require(endpoint.rstrip('/')=='https://'+cluster['endpoint'], 'Kubeconfig does not point to the verified cluster')
    require(kread(kubeconfig,['get','pv','-o','json']).get('items')==[], 'Persistent volumes remain. Delete only demo namespaces and wait for their disks to disappear before cleanup')
    services=kread(kubeconfig,['get','services','--all-namespaces','-o','json'])['items']
    require(not any(s['spec'].get('type')=='LoadBalancer' for s in services), 'LoadBalancer Services remain. Delete only demo namespaces and wait for load balancers to disappear before cleanup')

def execute(state, confirm, kubeconfig):
    require(confirm==state['run'],'--confirm-run must exactly match the captured run')
    client=GCloud(state['project']); current={}
    # Validate every extant recorded object before the first deletion.
    for kind,argv in specs(state).items():
        value=client.run(argv,absent_ok=True); current[kind]=value
        if value is not None:
            require(identity(kind,value,state)==state['ids'][kind], 'Resource was recreated or replaced: '+kind)
    if current['cluster'] is not None:
        require(bool(kubeconfig),'--host-kubeconfig is required for Kubernetes preflight')
        ensure_drained(current['cluster'],kubeconfig)
    run=state['run']
    if current.get('private_vm') is not None:
        client.run(['compute','instances','delete',run+'-a100','--zone='+state['private_zone'],'--delete-disks=boot'])
    if current['cluster'] is not None:
        client.run(['container','clusters','delete',run,'--zone='+state['zone']])
    if current['subnet'] is not None:
        client.run(['compute','networks','subnets','delete',run+'-subnet','--region='+state['region']])
    if current['network'] is not None:
        client.run(['compute','networks','delete',run+'-net'])
    if current['service_account'] is not None:
        email=run+'-node@'+state['project']+'.iam.gserviceaccount.com'
        # The dedicated SA and exact role are the only IAM changes in this recipe.
        policy=client.run(['projects','get-iam-policy',state['project']])
        member='serviceAccount:'+email; role='roles/container.defaultNodeServiceAccount'
        if any(b.get('role')==role and member in b.get('members',[]) and not b.get('condition') for b in policy.get('bindings',[])):
            client.run(['projects','remove-iam-policy-binding',state['project'],'--member='+member,'--role='+role,'--condition=None'])
        client.run(['iam','service-accounts','delete',email])
    missing=[]
    for kind,argv in specs(state).items():
        require(client.run(argv,absent_ok=True) is None,'Deletion not confirmed: '+kind)
        missing.append(kind)
    return {'confirmed_absent':missing,'note':'Only captured resources checked. Review the project resource inventory for orphaned disks/IPs; no sweeping delete was performed.'}

def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--state',required=True)
    sub=p.add_subparsers(dest='action',required=True)
    c=sub.add_parser('capture')
    for key in ('project','run','zone','region'): c.add_argument('--'+key,required=True)
    c.add_argument('--private-zone')
    sub.add_parser('plan')
    e=sub.add_parser('execute'); e.add_argument('--confirm-run',required=True); e.add_argument('--host-kubeconfig')
    a=p.parse_args(); outside_git(a.state)
    if a.action=='capture': return capture(a)
    state=load(a.state)
    if a.action=='plan': print(json.dumps({'plan':state,'deletes':list(specs(state)),'executed':False},indent=2))
    else: print(json.dumps(execute(state,a.confirm_run,a.host_kubeconfig),indent=2))
if __name__=='__main__':
    try: main()
    except GuardError as e: print('Stopped: '+str(e),file=sys.stderr); sys.exit(2)
    except (OSError,ValueError,KeyError,TypeError): print('Invalid state or command output; no broad cleanup attempted.',file=sys.stderr); sys.exit(2)
