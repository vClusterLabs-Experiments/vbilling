"""Offline guard/behavior tests. No cloud or Stripe connection is performed."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

SCRIPTS=Path(__file__).resolve().parents[1]/'scripts'
sys.path.insert(0,str(SCRIPTS))
from demo_common import GuardError, outside_git, private_read, run_id

def load(name):
    spec=importlib.util.spec_from_file_location(name.replace('-','_'),SCRIPTS/(name+'.py'))
    module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module); return module
stripe=load('stripe-test'); cleanup=load('cleanup-gcp'); startup=load('private-startup')
RUN='vbdemo-20260101-120000'
STATE={'run':RUN,'tenant':RUN,'event':'vcluster_gpu_hours__nvidia_l4_timeslice_2',
       'customer':'cus_TEST','meter':'mtr_test_TEST','product':'prod_TEST','price':'price_TEST'}

def owned(kind, **fields):
    return dict(object=kind,livemode=False,metadata={'vbilling_demo_run':RUN,'vbilling_tenant_id':RUN},**fields)

def subscription(**fields):
    return owned('subscription', id='sub_TEST',customer='cus_TEST',collection_method='charge_automatically',
                 items={'data':[{'id':'si_TEST','price':'price_TEST'}],'has_more':False},**fields)

class FakeStripe:
    def __init__(self, fail_pause=False):
        self.calls=[]; self.sub=None; self.fail_pause=fail_pause
    def all(self,path,query=None):
        if path=='/v1/subscriptions': return [] if self.sub is None else [copy.deepcopy(self.sub)]
        raise AssertionError(path)
    def request(self,method,path,form=None,idem=None):
        self.calls.append((method,path,form,idem))
        if path=='/v1/customers/cus_TEST': return owned('customer',id='cus_TEST',balance=0)
        if path.endswith(('/payment_methods','/sources')): return {'data':[],'has_more':False}
        if path=='/v1/billing/meters/mtr_test_TEST': return owned('billing.meter',id='mtr_test_TEST',status='active',event_name=STATE['event'],default_aggregation={'formula':'sum'},customer_mapping={'type':'by_id','event_payload_key':'stripe_customer_id'},value_settings={'event_payload_key':'value'})
        if path=='/v1/prices/price_TEST': return owned('price',id='price_TEST',active=True,billing_scheme='per_unit',currency='usd',unit_amount_decimal='250',product='prod_TEST',recurring={'interval':'month','usage_type':'metered','meter':'mtr_test_TEST'})
        if path=='/v1/invoiceitems': return {'data':[],'has_more':False}
        if path=='/v1/subscriptions' and method=='POST':
            self.sub=subscription(status='active'); return copy.deepcopy(self.sub)
        if path=='/v1/subscriptions/sub_TEST':
            if method=='POST':
                if self.fail_pause: raise GuardError('simulated network uncertainty')
                self.sub['pause_collection']={'behavior':'keep_as_draft','resumes_at':None}
            elif method=='DELETE': self.sub['status']='canceled'; self.sub['pause_collection']=None
            return copy.deepcopy(self.sub)
        if path=='/v1/invoices/create_preview':
            return owned('invoice',customer='cus_TEST',attempted=False,currency='usd',total=2,amount_due=2,
                         lines={'data':[{'price':'price_TEST','amount':2,'currency':'usd','quantity_decimal':'0.008333333'}]})
        raise AssertionError((method,path))

class StripeTests(unittest.TestCase):
    def test_create_persist_then_pause_and_verify(self):
        client=FakeStripe(); state=dict(STATE); snapshots=[]
        stripe.create_pause(client,state,lambda:snapshots.append(copy.deepcopy(state)))
        self.assertEqual(snapshots[0]['subscription'],'sub_TEST')
        self.assertFalse(snapshots[0]['pause_verified']); self.assertTrue(snapshots[-1]['pause_verified'])
        writes=[x for x in client.calls if x[0]=='POST']
        self.assertEqual([x[1] for x in writes],['/v1/subscriptions','/v1/subscriptions/sub_TEST'])
        self.assertEqual(writes[1][2]['pause_collection[behavior]'],'keep_as_draft')
        self.assertFalse(any(c[1].endswith(('/pay','/finalize','/send')) for c in client.calls))
    def test_pause_failure_preserves_id_and_retry_does_not_recreate(self):
        client=FakeStripe(fail_pause=True); state=dict(STATE); saved=[]
        with self.assertRaises(GuardError): stripe.create_pause(client,state,lambda:saved.append(copy.deepcopy(state)))
        self.assertEqual(saved[-1]['subscription'],'sub_TEST'); self.assertFalse(saved[-1]['pause_verified'])
        client.fail_pause=False; stripe.create_pause(client,state,lambda:None)
        self.assertEqual(sum(c[0]=='POST' and c[1]=='/v1/subscriptions' for c in client.calls),1)
    def test_live_object_rejected(self):
        value=subscription(status='active'); value['livemode']=True
        with self.assertRaises(GuardError): stripe.check_sub(value,STATE,False)
    def test_wrong_tenant_rejected(self):
        value=subscription(status='active'); value['metadata']['vbilling_tenant_id']='different'
        with self.assertRaises(GuardError): stripe.check_sub(value,STATE,False)
    def test_unpaused_preview_rejected(self):
        client=FakeStripe(); client.sub=subscription(status='active'); state=dict(STATE,subscription='sub_TEST')
        with self.assertRaises(GuardError): stripe.preview(client,state)
        self.assertFalse(any(c[1]=='/v1/invoices/create_preview' for c in client.calls))
    def test_preview_is_nonfinalizing(self):
        client=FakeStripe(); state=dict(STATE); stripe.create_pause(client,state,lambda:None)
        result=stripe.preview(client,state); self.assertEqual(result['total_cents'],2); self.assertTrue(result['preview_only'])
    def test_cancel_does_not_invoice_and_is_retryable(self):
        client=FakeStripe(); state=dict(STATE); stripe.create_pause(client,state,lambda:None)
        stripe.cancel(client,state,lambda:None); stripe.cancel(client,state,lambda:None)
        writes=[c for c in client.calls if c[0]=='DELETE']
        self.assertEqual(len(writes),1); self.assertEqual(writes[0][2],{'invoice_now':'false','prorate':'false'})
    def test_live_key_and_open_permissions_rejected_before_network(self):
        with tempfile.TemporaryDirectory() as temp:
            path=Path(temp)/'key'; path.write_text('sk_'+'live_SYNTHETICONLY'); path.chmod(0o600)
            with self.assertRaises(GuardError): stripe.Client(path)
            path.write_text('sk_'+'test_SYNTHETICONLY123456'); path.chmod(0o644)
            with self.assertRaises(GuardError): stripe.Client(path)


class CatalogFake(FakeStripe):
    def __init__(self, uncertain_price=False):
        super().__init__(); self.products=[]; self.prices=[]; self.uncertain_price=uncertain_price
    def all(self,path,query=None):
        if path=='/v1/customers': return [owned('customer',id='cus_TEST')]
        if path=='/v1/billing/meters': return [owned('billing.meter',id='mtr_test_TEST',event_name=STATE['event'])]
        if path=='/v1/products': return copy.deepcopy(self.products)
        if path=='/v1/prices': return copy.deepcopy(self.prices)
        return super().all(path,query)
    def request(self,method,path,form=None,idem=None):
        if method=='POST' and path=='/v1/products':
            self.calls.append((method,path,form,idem)); result=owned('product',id='prod_TEST')
            self.products.append(result); return result
        if method=='GET' and path=='/v1/products/prod_TEST': return owned('product',id='prod_TEST')
        if method=='POST' and path=='/v1/prices':
            self.calls.append((method,path,form,idem)); result=owned('price',id='price_TEST')
            self.prices.append(result)
            if self.uncertain_price: self.uncertain_price=False; raise GuardError('uncertain create response')
            return result
        return super().request(method,path,form,idem)

class CatalogTests(unittest.TestCase):
    def test_catalog_reuses_owned_objects_on_retry(self):
        client=CatalogFake(); state={k:STATE[k] for k in ('run','tenant','event')}
        stripe.catalog(client,state,lambda:None); stripe.catalog(client,state,lambda:None)
        self.assertEqual(state['product'],'prod_TEST'); self.assertEqual(state['price'],'price_TEST')
        self.assertEqual([c[1] for c in client.calls if c[0]=='POST'],['/v1/products','/v1/prices'])
        form=next(c[2] for c in client.calls if c[1]=='/v1/prices')
        self.assertEqual(form['recurring[meter]'],'mtr_test_TEST'); self.assertEqual(form['unit_amount_decimal'],'250')
    def test_catalog_unknown_price_outcome_recovers_by_metadata(self):
        client=CatalogFake(uncertain_price=True); state={k:STATE[k] for k in ('run','tenant','event')}
        with self.assertRaises(GuardError): stripe.catalog(client,state,lambda:None)
        self.assertEqual(state['product'],'prod_TEST'); self.assertNotIn('price',state)
        stripe.catalog(client,state,lambda:None)
        self.assertEqual(state['price'],'price_TEST')
        self.assertEqual(sum(c[0]=='POST' and c[1]=='/v1/prices' for c in client.calls),1)

class LocalGuards(unittest.TestCase):
    def test_state_inside_git_rejected(self):
        with tempfile.TemporaryDirectory() as temp:
            (Path(temp)/'.git').mkdir()
            with self.assertRaises(GuardError): outside_git(Path(temp)/'key')
    def test_run_id(self):
        self.assertEqual(run_id(RUN),RUN)
        with self.assertRaises(GuardError): run_id('../production')
    def test_join_host_guard(self):
        raw='curl -fsSLk "https://10.240.0.5/node/join?token=synthetic" | sh -'
        self.assertIn('PRIVATE_NODE_JOIN_COMPLETE',startup.render(raw,'10.240.0.5'))
        platform_raw=raw.replace('10.240.0.5','platform.example.invalid')
        self.assertIn('PRIVATE_NODE_JOIN_COMPLETE',startup.render(platform_raw,['10.240.0.5','platform.example.invalid']))
        self.assertIn('PRIVATE_NODE_JOIN_COMPLETE',startup.render(raw,['10.240.0.5','platform.example.invalid']))
        with self.assertRaises(GuardError): startup.render(raw,'example.invalid')
        with self.assertRaises(GuardError): startup.render(raw+'; malicious','10.240.0.5')
    def test_native_launcher_fake_binary_configuration(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp); root.chmod(0o700)
            kube=root/'host.kubeconfig'; kube.write_text('synthetic'); kube.chmod(0o600)
            key=root/'stripe-key'; key.write_text('sk_'+'test_SYNTHETICONLY123456'); key.chmod(0o600)
            binary=root/'vbilling'
            binary.write_text('#!/usr/bin/env python3\nimport json,os\nfrom pathlib import Path\nkeys=["ADAPTERS","LISTEN_ADDR","WATCH_NAMESPACES","COLLECTION_INTERVAL","TENANT_API","STRIPE_AUTO_SUBSCRIBE","STRIPE_CANCEL_ON_REMOVE","STRIPE_SPLIT_METERS_BY"]\nPath(os.environ["RUN_DIR"],"seen.json").write_text(json.dumps({k:os.environ[k] for k in keys}))\n')
            binary.chmod(0o700)
            env=dict(os.environ,RUN_DIR=str(root),HOST_KUBECONFIG=str(kube),STRIPE_KEY_FILE=str(key),WATCH_NAMESPACE=RUN,CLUSTER_NAME=RUN,REGION='test-region')
            result=subprocess.run(['bash',str(SCRIPTS/'run-local.sh')],env=env,capture_output=True,text=True)
            self.assertEqual(result.returncode,0,result.stderr)
            config=json.loads((root/'seen.json').read_text())
            self.assertEqual(config['ADAPTERS'],'stripe'); self.assertEqual(config['TENANT_API'],'false')
            self.assertEqual(config['STRIPE_AUTO_SUBSCRIBE'],'false'); self.assertEqual(config['STRIPE_CANCEL_ON_REMOVE'],'false')
            self.assertNotIn(key.read_text(),result.stdout+result.stderr)
            self.assertFalse((root/'collector.lock').exists())
            self.assertEqual((root/'api-token').stat().st_mode & 0o777,0o600)
    def test_launcher_rejects_second_collector(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp); root.chmod(0o700)
            kube=root/'kube'; kube.write_text('synthetic'); kube.chmod(0o600)
            binary=root/'vbilling'; binary.write_text('#!/bin/sh\nexit 0\n'); binary.chmod(0o700)
            (root/'collector.lock').mkdir()
            env=dict(os.environ,RUN_DIR=str(root),HOST_KUBECONFIG=str(kube),WATCH_NAMESPACE=RUN,CLUSTER_NAME=RUN,REGION='test-region')
            env.pop('STRIPE_KEY_FILE',None)
            result=subprocess.run(['bash',str(SCRIPTS/'run-local.sh')],env=env,capture_output=True,text=True)
            self.assertEqual(result.returncode,2); self.assertIn('Collector lock exists',result.stderr)

CSTATE={'run':RUN,'project':'example-project','zone':'us-central1-a','region':'us-central1','private_zone':None}
def cloud_values(state):
    return {
        'cluster':{'id':'cluster-id','name':RUN,'location':state['zone'],'resourceLabels':{'run':RUN,'purpose':'vbilling-demo'},'network':RUN+'-net','subnetwork':RUN+'-subnet','endpoint':'192.0.2.1'},
        'network':{'id':'network-id','name':RUN+'-net','description':'vbilling-demo:'+RUN},
        'subnet':{'id':'subnet-id','name':RUN+'-subnet','description':'vbilling-demo:'+RUN,'network':'https://example/projects/example-project/global/networks/'+RUN+'-net'},
        'service_account':{'uniqueId':'sa-id','email':RUN+'-node@example-project.iam.gserviceaccount.com','displayName':'vBilling demo '+RUN},
    }
class FakeGCloud:
    def __init__(self,state,values): self.state=state; self.values=values; self.calls=[]
    def run(self,args,absent_ok=False):
        self.calls.append(args)
        for kind,argv in cleanup.specs(self.state).items():
            if args==argv: return copy.deepcopy(self.values.get(kind))
        if args[:2]==['projects','get-iam-policy']: return {'bindings':[]}
        require_delete='delete' in args
        if require_delete:
            for kind,argv in cleanup.specs(self.state).items():
                if args[:args.index('delete')]==argv[:argv.index('describe')]: self.values.pop(kind,None); return {}
        raise AssertionError(args)
class CleanupTests(unittest.TestCase):
    def setUp(self):
        self.state=dict(CSTATE); self.values=cloud_values(self.state)
        self.state['ids']={k:cleanup.identity(k,v,self.state) for k,v in self.values.items()}
    def test_plan_is_no_network(self):
        self.assertEqual(set(cleanup.specs(self.state)),set(self.state['ids']))
    def test_wrong_confirmation_blocks(self):
        with self.assertRaises(GuardError): cleanup.execute(self.state,'different','ignored')
    def test_replaced_resource_blocks_all_deletes(self):
        self.values['network']['id']='replacement'; fake=FakeGCloud(self.state,self.values)
        with patch.object(cleanup,'GCloud',return_value=fake):
            with self.assertRaises(GuardError): cleanup.execute(self.state,RUN,'ignored')
        self.assertFalse(any('delete' in c for c in fake.calls))
    def test_owner_label_blocks_all_deletes(self):
        self.values['cluster']['resourceLabels']['run']='different'; fake=FakeGCloud(self.state,self.values)
        with patch.object(cleanup,'GCloud',return_value=fake):
            with self.assertRaises(GuardError): cleanup.execute(self.state,RUN,'ignored')
        self.assertFalse(any('delete' in c for c in fake.calls))
    def test_pv_preflight_failure_blocks(self):
        fake=FakeGCloud(self.state,self.values)
        with patch.object(cleanup,'GCloud',return_value=fake),patch.object(cleanup,'ensure_drained',side_effect=GuardError('PV exists')):
            with self.assertRaises(GuardError): cleanup.execute(self.state,RUN,'ignored')
        self.assertFalse(any('delete' in c for c in fake.calls))
    def test_exact_delete_order_and_repeated_absent(self):
        fake=FakeGCloud(self.state,self.values)
        with patch.object(cleanup,'GCloud',return_value=fake),patch.object(cleanup,'ensure_drained'):
            result=cleanup.execute(self.state,RUN,'ignored')
            cleanup.execute(self.state,RUN,'ignored')
        self.assertEqual(len(result['confirmed_absent']),4)
        deletes=[c for c in fake.calls if 'delete' in c]
        self.assertEqual(len(deletes),4)
        self.assertEqual(deletes[0][:3],['container','clusters','delete'])
        self.assertTrue(all('--filter' not in ' '.join(c) for c in deletes))
    def test_private_vm_only_exact_boot_disk_and_network(self):
        state=dict(self.state,private_zone='us-central1-f')
        good={'id':'vm-id','name':RUN+'-a100','zone':'projects/example-project/zones/us-central1-f',
              'labels':{'run':RUN,'purpose':'vbilling-demo'},
              'disks':[{'boot':True,'autoDelete':True}],
              'networkInterfaces':[{'network':'projects/example-project/global/networks/'+RUN+'-net'}]}
        self.assertEqual(cleanup.identity('private_vm',good,state),'vm-id')
        for changed in ({'disks':[{'boot':True,'autoDelete':False}]}, {'disks':[{'boot':False,'autoDelete':True}]},
                        {'disks':[{'boot':True,'autoDelete':True},{'boot':False,'autoDelete':False}]},
                        {'networkInterfaces':[]}, {'networkInterfaces':[{'network':'production'}]}):
            with self.subTest(changed=changed), self.assertRaises(GuardError):
                cleanup.identity('private_vm',dict(good,**changed),state)
    def test_gke_precise_404_is_absence(self):
        proc=subprocess.CompletedProcess([],1,'','ResponseError: code=404, message=Not found: projects/example-project/locations/us-central1-a/clusters/demo. No cluster named demo.')
        with patch.object(cleanup.subprocess,'run',return_value=proc):
            self.assertIsNone(cleanup.GCloud('example-project').run(['container','clusters','describe','demo'],absent_ok=True))
    def test_iam_ambiguous_describe_uses_successful_empty_list(self):
        email=RUN+'-node@example-project.iam.gserviceaccount.com'
        denied=subprocess.CompletedProcess([],1,'','PERMISSION_DENIED: Permission denied on resource (or it may not exist).')
        empty=subprocess.CompletedProcess([],0,'[]','')
        with patch.object(cleanup.subprocess,'run',side_effect=[denied,empty]) as mock:
            self.assertIsNone(cleanup.GCloud('example-project').run(['iam','service-accounts','describe',email],absent_ok=True))
        self.assertEqual(mock.call_args_list[1].args[0][1:5],['iam','service-accounts','list','--filter=email='+email])
    def test_iam_failed_list_does_not_confirm_absence(self):
        email=RUN+'-node@example-project.iam.gserviceaccount.com'
        denied=subprocess.CompletedProcess([],1,'','PERMISSION_DENIED: Permission denied on resource (or it may not exist).')
        with patch.object(cleanup.subprocess,'run',side_effect=[denied,denied]):
            with self.assertRaises(GuardError): cleanup.GCloud('example-project').run(['iam','service-accounts','describe',email],absent_ok=True)
    def test_iam_existing_account_does_not_confirm_absence(self):
        email=RUN+'-node@example-project.iam.gserviceaccount.com'
        denied=subprocess.CompletedProcess([],1,'','PERMISSION_DENIED: Permission denied on resource (or it may not exist).')
        present=subprocess.CompletedProcess([],0,json.dumps([{'email':email}]),'')
        with patch.object(cleanup.subprocess,'run',side_effect=[denied,present]):
            with self.assertRaises(GuardError): cleanup.GCloud('example-project').run(['iam','service-accounts','describe',email],absent_ok=True)
    def test_other_resource_ambiguous_permission_never_lists_iam(self):
        denied=subprocess.CompletedProcess([],1,'','PERMISSION_DENIED: resource was not found (or it may not exist).')
        with patch.object(cleanup.subprocess,'run',return_value=denied) as mock:
            with self.assertRaises(GuardError): cleanup.GCloud('example-project').run(['compute','networks','describe','x'],absent_ok=True)
        self.assertEqual(mock.call_count,1)
    def test_permission_error_is_not_absence(self):
        proc=subprocess.CompletedProcess([],1,'','PERMISSION_DENIED')
        with patch.object(cleanup.subprocess,'run',return_value=proc):
            with self.assertRaises(GuardError): cleanup.GCloud('example-project').run(['compute','networks','describe','x'],absent_ok=True)

if __name__=='__main__': unittest.main()
