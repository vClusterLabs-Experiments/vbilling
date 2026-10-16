#!/usr/bin/env python3
"""Test-only Stripe administration; the Go vBilling adapter sends all usage.

No payment method, charge, pay, send, finalize, or resume command is implemented.
Creating and pausing a subscription takes TWO requests. Stripe may finalize its
initial zero-value invoice before the pause. Retry the SAME state after any
uncertain result; the subscription ID is saved before the pause request.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from demo_common import GuardError, outside_git, private_read, provider_id, require, run_id, save_json

BASE = 'https://api.stripe.com'
VERSION = '2026-09-30.endive'
META = 'vbilling_demo_run'

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise GuardError('Stripe redirect refused')

class Client:
    def __init__(self, key_file):
        self.key = private_read(key_file)
        require(re.fullmatch(r'sk_test_[A-Za-z0-9]+', self.key) and len(self.key) > 16,
                'Only Stripe test secret keys are accepted')
        self.opener = urllib.request.build_opener(NoRedirect)

    def request(self, method, path, form=None, idem=None):
        require(path.startswith('/v1/') and not any(x in path for x in ('://', '..', '#', '?')),
                'Unexpected Stripe API path')
        data = urllib.parse.urlencode(form or {}).encode()
        url = BASE + path
        headers = {'Authorization': 'Bearer ' + self.key, 'Stripe-Version': VERSION}
        if idem: headers['Idempotency-Key'] = idem
        if method == 'GET':
            if data: url += '?' + data.decode()
            data = None
        else: headers['Content-Type'] = 'application/x-www-form-urlencoded'
        try:
            with self.opener.open(urllib.request.Request(url, data=data, headers=headers, method=method), timeout=30) as response:
                result = json.load(response)
        except urllib.error.HTTPError as error:
            raise GuardError('Stripe HTTP ' + str(error.code) + '; response suppressed. Retry with the SAME state file.') from None
        except (urllib.error.URLError, TimeoutError, OSError, ValueError):
            raise GuardError('Stripe outcome uncertain; response suppressed. Retry with the SAME state file.') from None
        require(isinstance(result, dict), 'Unexpected Stripe response')
        require(result.get('livemode') is not True, 'A live-mode object was returned; refusing')
        return result

    def all(self, path, query=None):
        query = dict(query or {}, limit=100)
        rows = []
        for _ in range(100):
            result = self.request('GET', path, query)
            batch = result.get('data')
            require(isinstance(batch, list), 'Invalid Stripe list')
            require(all(r.get('livemode') is not True for r in batch), 'Live-mode object in list')
            rows.extend(batch)
            if not result.get('has_more'): return rows
            require(bool(batch), 'Invalid empty Stripe page')
            query['starting_after'] = batch[-1]['id']
        raise GuardError('Stripe pagination limit reached; narrow the demo account')

def idem(state, action):
    raw = '|'.join([state['run'], state['tenant'], state['event'], action])
    return 'vbilling-demo-' + hashlib.sha256(raw.encode()).hexdigest()

def obj(value, kind):
    require(value.get('object') == kind and value.get('livemode') is False,
            'Expected an explicitly test-mode ' + kind)
    return value

def ref(value):
    return value.get('id') if isinstance(value, dict) else value

def owned(value, state, kind):
    value = obj(value, kind)
    require((value.get('metadata') or {}).get(META) == state['run'], 'Object is not owned by this run')
    require((value.get('metadata') or {}).get('vbilling_tenant_id') == state['tenant'], 'Object tenant differs')
    return value

def one(rows, message):
    require(len(rows) == 1, message)
    return rows[0]

def check_customer(client, state):
    customer = obj(client.request('GET', '/v1/customers/' + provider_id(state['customer'], 'cus_')), 'customer')
    require((customer.get('metadata') or {}).get('vbilling_tenant_id') == state['tenant'], 'Customer tenant does not match')
    require(not customer.get('email'), 'Demo customer must have no email')
    require(not customer.get('default_source') and not (customer.get('invoice_settings') or {}).get('default_payment_method'),
            'Demo customer must have no default payment source')
    require(customer.get('balance', 0) == 0 and not customer.get('discount') and not customer.get('discounts'),
            'Demo customer must have no credit balance or discounts')
    for suffix in ('payment_methods', 'sources'):
        result = client.request('GET', '/v1/customers/' + state['customer'] + '/' + suffix, {'limit': 1})
        require(result.get('data') == [] and not result.get('has_more'), 'Demo customer must have no saved payment methods')
    return customer

def check_meter(client, state):
    meter = obj(client.request('GET', '/v1/billing/meters/' + provider_id(state['meter'], 'mtr_')), 'billing.meter')
    require(meter.get('event_name') == state['event'] and meter.get('status') == 'active', 'Meter event or status differs')
    require((meter.get('default_aggregation') or {}).get('formula') == 'sum', 'Meter must SUM usage')
    require(meter.get('customer_mapping') == {'type': 'by_id', 'event_payload_key': 'stripe_customer_id'} and
            (meter.get('value_settings') or {}).get('event_payload_key') == 'value', 'Meter payload keys differ')
    return meter

def check_price(client, state):
    price = owned(client.request('GET', '/v1/prices/' + provider_id(state['price'], 'price_')), state, 'price')
    recurring = price.get('recurring') or {}
    require(price.get('currency') == 'usd' and str(price.get('unit_amount_decimal')) == '250', 'Expected USD 250 cents per GPU-hour')
    require(price.get('active') and price.get('billing_scheme') == 'per_unit' and not price.get('transform_quantity'), 'Price must be active per-unit without transform')
    require(recurring.get('meter') == state['meter'] and recurring.get('interval') == 'month' and
            recurring.get('interval_count', 1) == 1 and recurring.get('usage_type') == 'metered', 'Price meter or billing interval differs')
    require(ref(price.get('product')) == state['product'], 'Price product differs')
    return price

def check_sub(sub, state, paused=True):
    sub = owned(sub, state, 'subscription')
    provider_id(sub.get('id'), 'sub_')
    require(ref(sub.get('customer')) == state['customer'], 'Subscription customer differs')
    require(sub.get('collection_method') == 'charge_automatically', 'Subscription collection method differs')
    require(not sub.get('default_source') and not sub.get('default_payment_method'), 'Subscription has a payment source')
    require(not sub.get('discounts') and not sub.get('discount') and not sub.get('trial_end') and
            not sub.get('default_tax_rates') and not (sub.get('automatic_tax') or {}).get('enabled'), 'Subscription has unexpected adjustments')
    items = sub.get('items') or {}
    rows = items.get('data') or []
    require(len(rows) == 1 and not items.get('has_more') and ref(rows[0].get('price')) == state['price'], 'Subscription must contain only this run price')
    if paused:
        pause = sub.get('pause_collection') or {}
        require(pause.get('behavior') == 'keep_as_draft' and pause.get('resumes_at') is None,
                'Collection is not confirmed paused. Retry create-and-pause with the same state immediately.')
    return sub

def metadata(state):
    return {'metadata[' + META + ']': state['run'], 'metadata[vbilling_tenant_id]': state['tenant'],
            'metadata[vbilling_plan]': state['run']}

def catalog(client, state, save):
    if not state.get('customer'):
        row = one([c for c in client.all('/v1/customers') if (c.get('metadata') or {}).get('vbilling_tenant_id') == state['tenant']],
                  'Need exactly one vBilling-created test customer for this tenant; start collector and wait')
        state['customer'] = provider_id(row['id'], 'cus_'); save()
    check_customer(client, state)
    if not state.get('meter'):
        row = one([m for m in client.all('/v1/billing/meters') if m.get('event_name') == state['event']],
                  'Need exactly one vBilling-created meter for this event; wait for its first measurement')
        state['meter'] = provider_id(row['id'], 'mtr_'); save()
    check_meter(client, state)
    if not state.get('product'):
        rows = [p for p in client.all('/v1/products') if (p.get('metadata') or {}).get(META) == state['run'] and
                (p.get('metadata') or {}).get('vbilling_tenant_id') == state['tenant']]
        require(len(rows) <= 1, 'Duplicate run products; inspect before continuing')
        product = rows[0] if rows else client.request('POST', '/v1/products', dict(metadata(state), name='GPU access example ' + state['tenant']), idem(state, 'product'))
        owned(product, state, 'product')
        state['product'] = provider_id(product['id'], 'prod_'); save()
    owned(client.request('GET', '/v1/products/' + provider_id(state['product'], 'prod_')), state, 'product')
    if not state.get('price'):
        rows = [p for p in client.all('/v1/prices', {'product': state['product']}) if (p.get('metadata') or {}).get(META) == state['run']]
        require(len(rows) <= 1, 'Duplicate run prices; inspect before continuing')
        price = rows[0] if rows else client.request('POST', '/v1/prices', dict(metadata(state), **{
            'product': state['product'], 'currency': 'usd', 'unit_amount_decimal': '250',
            'recurring[interval]': 'month', 'recurring[usage_type]': 'metered', 'recurring[meter]': state['meter']}), idem(state, 'price'))
        owned(price, state, 'price')
        state['price'] = provider_id(price['id'], 'price_'); save()
    check_price(client, state)
    return dict(state, note='Product and USD 2.50/GPU-hour price ready. Meter/customer came from vBilling; no subscription created yet.')

def create_pause(client, state, save):
    check_customer(client, state); check_meter(client, state); check_price(client, state)
    rows = client.all('/v1/subscriptions', {'customer': state['customer'], 'status': 'all'})
    ours = [s for s in rows if (s.get('metadata') or {}).get(META) == state['run']]
    require(len(ours) <= 1 and len(rows) == len(ours), 'Customer has other subscriptions; use an isolated demo tenant')
    if state.get('subscription'):
        sub = client.request('GET', '/v1/subscriptions/' + provider_id(state['subscription'], 'sub_'))
    elif ours:
        sub = ours[0]
    else:
        pending = client.request('GET', '/v1/invoiceitems', {'customer': state['customer'], 'pending': 'true', 'limit': 1})
        require(pending.get('data') == [] and not pending.get('has_more'), 'Customer has pending invoice items')
        sub = client.request('POST', '/v1/subscriptions', dict(metadata(state), **{
            'customer': state['customer'], 'items[0][price]': state['price'],
            'collection_method': 'charge_automatically', 'payment_behavior': 'default_incomplete',
            'proration_behavior': 'none', 'automatic_tax[enabled]': 'false',
            'payment_settings[save_default_payment_method]': 'off'}), idem(state, 'subscription'))
    sub = check_sub(sub, state, paused=False)
    require(sub.get('status') not in ('canceled', 'incomplete_expired'), 'Run subscription is canceled/expired; never recreate it under the same run')
    state['subscription'] = sub['id']; state['pause_verified'] = False; save()
    # First network call after the new subscription's identity is persisted.
    # Do not log/debug a raw provider response or add calls before the pause.
    client.request('POST', '/v1/subscriptions/' + sub['id'],
                   {'pause_collection[behavior]': 'keep_as_draft', 'proration_behavior': 'none'}, idem(state, 'pause'))
    sub = check_sub(client.request('GET', '/v1/subscriptions/' + sub['id']), state)
    require(sub.get('status') == 'active', 'Collection paused but subscription is not active; inspect test account')
    state['pause_verified'] = True; save()
    return dict(state, note='Test collection verified paused indefinitely. Creation + pause were two requests; initial zero invoice may have finalized.')

def inspect(client, state):
    check_customer(client, state); check_meter(client, state); check_price(client, state)
    sub = check_sub(client.request('GET', '/v1/subscriptions/' + provider_id(state.get('subscription'), 'sub_')), state)
    return dict(state, status=sub.get('status'), pause_collection=sub['pause_collection'])

def preview(client, state):
    inspect(client, state)
    invoice = obj(client.request('POST', '/v1/invoices/create_preview', {
        'customer': state['customer'], 'subscription': state['subscription']}), 'invoice')
    require(ref(invoice.get('customer')) == state['customer'], 'Preview customer differs')
    require(invoice.get('attempted') is not True, 'Unexpected payment-attempt flag on preview')
    lines = invoice.get('lines') or {}
    require(not lines.get('has_more'), 'Preview has additional lines; do not claim a verified total')
    cleaned = []
    for row in lines.get('data', []):
        price = ref(((row.get('pricing') or {}).get('price_details') or {}).get('price')) or ref(row.get('price'))
        require(price == state['price'], 'Preview contains another price')
        cleaned.append({key: row.get(key) for key in ('amount', 'currency', 'quantity', 'quantity_decimal', 'period')})
    return {'preview_only': True, 'livemode': False, 'customer': state['customer'], 'subscription': state['subscription'],
            'amount_due_cents': invoice.get('amount_due'), 'total_cents': invoice.get('total'), 'currency': invoice.get('currency'), 'lines': cleaned}

def cancel(client, state, save):
    sid = provider_id(state.get('subscription'), 'sub_')
    sub = check_sub(client.request('GET', '/v1/subscriptions/' + sid), state, paused=False)
    if sub.get('status') != 'canceled':
        sub = check_sub(client.request('DELETE', '/v1/subscriptions/' + sid, {'invoice_now': 'false', 'prorate': 'false'}), state, paused=False)
    require(sub.get('status') == 'canceled', 'Cancellation not confirmed; retry same state')
    state['canceled'] = True; save()
    return dict(state, note='Exact owned test subscription canceled, without an invoice or proration. Customer, meter, product and price retained as evidence.')

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--key-file', required=True)
    p.add_argument('--state', required=True)
    p.add_argument('--run', required=True)
    p.add_argument('--tenant', required=True)
    p.add_argument('--event', required=True)
    p.add_argument('action', choices=['catalog', 'create-and-pause', 'inspect', 'preview', 'cancel'])
    a = p.parse_args()
    run_id(a.run)
    require(a.tenant in (a.run, a.run + '-private'), 'Tenant must be the run ID or run ID-private')
    require(a.event in ('vcluster_gpu_hours__nvidia_l4_timeslice_2', 'vcluster_gpu_hours__nvidia_a100_sxm4_40gb'), 'Use one of the documented demo GPU events')
    path = outside_git(a.state)
    identity = {'run': a.run, 'tenant': a.tenant, 'event': a.event}
    state = json.loads(private_read(path)) if path.exists() else dict(identity)
    require(all(state.get(k) == v for k, v in identity.items()), 'Existing state belongs to a different run, tenant, or meter')
    save = lambda: save_json(path, state)
    if not path.exists(): save()
    client = Client(a.key_file)
    if a.action == 'catalog': result = catalog(client, state, save)
    elif a.action == 'create-and-pause': result = create_pause(client, state, save)
    elif a.action == 'cancel': result = cancel(client, state, save)
    elif a.action == 'preview': result = preview(client, state)
    else: result = inspect(client, state)
    print(json.dumps(result, indent=2))

if __name__ == '__main__':
    try: main()
    except GuardError as error:
        print('Stopped: ' + str(error), file=sys.stderr); sys.exit(2)
    except (OSError, ValueError, KeyError, TypeError):
        print('Stopped: invalid or incomplete private state; raw data suppressed. Preserve state and inspect locally.', file=sys.stderr); sys.exit(2)
