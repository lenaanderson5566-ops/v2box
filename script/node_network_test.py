import importlib.util
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('network', Path(__file__).with_name('node-network.py'))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)

class FakeClient:
    def __init__(self, records=None, ipv4='8.8.8.8', ipv6='2606:4700:4700::1111'):
        self.records = records or []
        self.ipv4, self.ipv6 = ipv4, ipv6
        self.calls = []
    def request(self, url):
        return self.ipv6 if url in m.IP_SERVICES['AAAA'] else self.ipv4
    def cloudflare(self, config, path='', method='GET', data=None):
        self.calls.append((path, method, data))
        return {'success': True, 'result': self.records if method == 'GET' else {}}

class NetworkTests(unittest.TestCase):
    def setUp(self):
        self.config = {'zone_id': 'a'*32, 'token': 'secret-test-token', 'domain': 'node.example.com', 'family':'ipv4'}
    def test_capabilities_can_be_probed_without_root_or_configuration(self):
        import io
        with patch.object(m.sys,'argv',['node-network','capabilities']),patch.object(m.sys,'stdout',io.StringIO()) as output:
            self.assertEqual(m.main(),0)
            self.assertIn('report-multi-target-v1',json.loads(output.getvalue())['features'])
    def test_panel_report_scoped_and_partial(self):
        config = {'mode':'panel','panel_url':'https://panel.example.test','token':'a'*64,'family':'dual'}
        c = m.Client()
        def request(url, method='GET', data=None, token=None):
            if url in m.IP_SERVICES['A']: return '8.8.8.8'
            if url in m.IP_SERVICES['AAAA']: raise m.NetworkError('no ipv6')
            self.assertEqual(url, 'https://panel.example.test/api/v10/server/addresses')
            self.assertEqual((method, data, token), ('POST', {'ipv4':'8.8.8.8'}, 'a'*64))
            return '{"data":{"accepted":true}}'
        with patch.object(c, 'request', side_effect=request) as call:
            m.execute(c, config)
            self.assertEqual(call.call_count, 4)
    def test_panel_url_and_key_validation(self):
        base = {'panel_url':'https://panel.example.test','token':'a'*64,'family':'ipv4'}
        for url in ['http://panel.example.test','https://user:pass@panel.example.test','https://panel.example.test/path','https://panel.example.test?token=x']:
            with self.assertRaises(m.NetworkError): m.validate_report(dict(base, panel_url=url))
        with self.assertRaises(m.NetworkError): m.validate_report(dict(base, token='cloudflare-token'))
    def test_report_without_addresses_does_not_send(self):
        config = {'mode':'panel','panel_url':'https://panel.example.test','token':'a'*64,'family':'ipv4'}
        c=m.Client()
        with patch.object(c, 'request', return_value='127.0.0.1') as call:
            with self.assertRaises(m.NetworkError): m.report(c, config)
            self.assertEqual(call.call_count, 2)
    def test_report_rejects_malformed_ack(self):
        config = {'mode':'panel','panel_url':'https://panel.example.test','token':'a'*64,'family':'ipv4'}
        c=m.Client()
        with patch.object(c, 'request', side_effect=['8.8.8.8','{"data":{}}']):
            with self.assertRaises(m.NetworkError): m.report(c, config)
    def test_create_dns_only(self):
        c=FakeClient();m.sync(c,self.config)
        self.assertEqual(c.calls[-1][1], 'POST')
        self.assertEqual(c.calls[-1][2], {'type':'A','name':'node.example.com','content':'8.8.8.8','ttl':300,'proxied':False})
    def test_unchanged_skips_write(self):
        c=FakeClient([{'id':'b'*32,'name':'node.example.com','type':'A','content':'8.8.8.8','proxied':False}]);m.sync(c,self.config)
        self.assertEqual(len(c.calls),1)
    def test_patch_preserves_unrelated_fields(self):
        c=FakeClient([{'id':'b'*32,'name':'node.example.com','type':'A','content':'9.9.9.9','proxied':False,'comment':'keep'}]);m.sync(c,self.config)
        self.assertEqual(c.calls[-1][1], 'PATCH')
        self.assertNotIn('comment',c.calls[-1][2])
    def test_refuse_ambiguous_record(self):
        c=FakeClient([{},{}])
        with self.assertRaises(m.NetworkError) as caught:m.sync(c,self.config)
        self.assertTrue(caught.exception.permanent)
        self.assertEqual(len(c.calls),1)
    def test_ipv6_record(self):
        c=FakeClient();m.sync(c,dict(self.config,family='ipv6'))
        self.assertEqual(c.calls[-1][2]['type'],'AAAA')
    def test_missing_family_does_not_delete(self):
        c=FakeClient(ipv6='not-an-ip')
        with self.assertRaises(m.NetworkError):m.sync(c,dict(self.config,family='dual'))
        self.assertEqual([v[1] for v in c.calls],['GET','POST'])
    def test_reject_nonpublic_and_wrong_family(self):
        for address in ['127.0.0.1','10.0.0.1','0.0.0.0','224.0.0.1','::1','<html>error</html>']:
            with self.assertRaises(m.NetworkError):m.discover(FakeClient(ipv4=address),'A')
    def test_fallback_after_failure(self):
        c=FakeClient()
        with patch.object(c,'request',side_effect=[m.NetworkError('unavailable'),'1.1.1.1']):
            self.assertEqual(m.discover(c,'A'),'1.1.1.1')
    def test_configuration_validation(self):
        for changes in [{'zone_id':'../other-zone'},{'domain':'8.8.8.8'},{'domain':'x;touch /tmp/a'},{'token':'bad\nvalue'},{'family':'all'}]:
            with self.assertRaises(m.NetworkError):m.validate(dict(self.config,**changes))
        self.assertEqual(m.validate(dict(self.config,domain='NODE.Example.Com.'))['domain'],'node.example.com')
    def test_http_auth_and_rate_limit(self):
        import urllib.error
        for code,permanent in [(401,True),(403,True),(429,False),(500,False)]:
            error=urllib.error.HTTPError('https://example.test',code,'test',{'Retry-After':'600'},None)
            with patch.object(m.urllib.request,'build_opener') as opener:
                opener.return_value.open.side_effect=error
                with self.assertRaises(m.NetworkError) as caught:m.Client().request('https://example.test',token='secret')
                self.assertEqual(caught.exception.permanent,permanent)
                self.assertEqual(caught.exception.retry_after,600)
                self.assertNotIn('secret',str(caught.exception))
    def test_systemd_enable_disable(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp)
            (root/'run/systemd/system').mkdir(parents=True)
            (root/'etc/systemd/system').mkdir(parents=True)
            with patch.object(m,'Path',side_effect=lambda value:root/str(value).lstrip('/')),patch.object(m.subprocess,'run') as run:
                m.schedule(True)
                self.assertIn('OnUnitActiveSec=5min',(root/'etc/systemd/system/node-network.timer').read_text())
                self.assertIn('node-network.py run',(root/'etc/systemd/system/node-network.service').read_text())
                m.schedule(False)
                self.assertEqual(run.call_args.args[0],['systemctl','disable','--now','node-network.timer'])
    def test_openrc_preserves_other_cron_jobs(self):
        result=m.subprocess.CompletedProcess([],0,'0 1 * * * existing-job\nold-command # node-network-ddns\n','')
        with patch.object(m.Path,'exists',return_value=False),patch.object(m.shutil,'which',return_value='/usr/bin/tool'),patch.object(m.subprocess,'run',return_value=result) as run:
            m.schedule(True)
            text=run.call_args.kwargs['input']
            self.assertIn('existing-job',text)
            self.assertNotIn('old-command',text)
            self.assertEqual(text.count('# node-network-ddns'),1)
            m.schedule(False)
            self.assertEqual(run.call_args.kwargs['input'],'0 1 * * * existing-job\n')
    @unittest.skipUnless(m.sys.platform=='linux','Linux lock and file permissions')
    def test_auth_failure_pauses_scheduled_runs(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp)
            with patch.object(m,'CONFIG',root/'config'),patch.object(m,'STATE',root/'state'):
                m.write_private(m.CONFIG,self.config)
                self.assertEqual(m.CONFIG.stat().st_mode & 0o777,0o600)
                with patch.object(m,'sync',side_effect=m.NetworkError('denied',True)) as sync:
                    with self.assertRaises(m.NetworkError):m.run()
                    m.run()
                    self.assertEqual(sync.call_count,1)
                self.assertTrue(json.loads(m.STATE.read_text())['paused'])
    @unittest.skipUnless(m.sys.platform=='linux','Linux lock and file permissions')
    def test_rate_limit_backoff(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp)
            with patch.object(m,'CONFIG',root/'config'),patch.object(m,'STATE',root/'state'):
                m.write_private(m.CONFIG,self.config)
                with patch.object(m,'sync',side_effect=m.NetworkError('rate limited',False,900)) as sync:
                    with self.assertRaises(m.NetworkError):m.run()
                    m.run()
                    self.assertEqual(sync.call_count,1)
                self.assertFalse(json.loads(m.STATE.read_text())['paused'])

class MultiPanelNetworkTests(unittest.TestCase):
    def setUp(self):
        self.a={'mode':'panel','panel_url':'https://a.example.test','token':'a'*64,'family':'ipv4','server_id':1}
        self.b=dict(self.a,panel_url='https://b.example.test',token='b'*64)
    def test_merge_preserves_other_panel_and_direct_dns(self):
        dns={'zone_id':'c'*32,'token':'dns-key','domain':'node.example.test','family':'ipv4'}
        document=m.merge_target(dns,self.a)
        document=m.merge_target(document,self.b)
        document=m.merge_target(document,dict(self.a,token='d'*64))
        self.assertEqual(len(m.targets(document)),3)
        self.assertEqual(m.targets(document)[0],dns)
        self.assertIn(self.b,m.targets(document))
        self.assertNotIn(self.a,m.targets(document))
    def test_same_node_id_on_different_panels_has_distinct_identity(self):
        self.assertNotEqual(m.target_key(self.a),m.target_key(self.b))
        self.assertEqual(m.target_key(self.a),m.target_key(dict(self.a,token='c'*64)))
    def test_failed_target_does_not_pause_other_backend(self):
        with tempfile.TemporaryDirectory() as temp,patch.object(m,'STATE',Path(temp)/'state'):
            calls=[]
            def execute(client,config):
                calls.append(config['panel_url'])
                if config['panel_url']==self.a['panel_url']:raise m.NetworkError('denied',True)
            document=m.merge_target(self.a,self.b)
            with patch.object(m,'execute',side_effect=execute):
                with self.assertRaises(m.NetworkError):m.run_multiple(document,{})
                state=json.loads(m.STATE.read_text())
                self.assertTrue(state['targets'][m.target_key(self.a)]['paused'])
                self.assertFalse(state['targets'][m.target_key(self.b)].get('paused',False))
                m.run_multiple(document,state)
            self.assertEqual(calls.count(self.a['panel_url']),1)
            self.assertEqual(calls.count(self.b['panel_url']),2)
    def test_slow_target_does_not_delay_other_backend(self):
        import threading
        with tempfile.TemporaryDirectory() as temp,patch.object(m,'STATE',Path(temp)/'state'):
            slow=threading.Event();healthy=threading.Event();release=threading.Event()
            errors=[]
            def execute(client,config):
                if config['panel_url']==self.a['panel_url']:
                    slow.set();release.wait(3)
                else: healthy.set()
            def run():
                try:m.run_multiple(m.merge_target(self.a,self.b),{})
                except BaseException as error:errors.append(error)
            with patch.object(m,'execute',side_effect=execute):
                thread=threading.Thread(target=run);thread.start()
                try:
                    self.assertTrue(slow.wait(1));self.assertTrue(healthy.wait(1))
                finally:release.set();thread.join(3)
            self.assertFalse(thread.is_alive());self.assertEqual(errors,[])
    def test_credential_rotation_preserves_other_backoff(self):
        with tempfile.TemporaryDirectory() as temp:
            with patch.object(m,'CONFIG',Path(temp)/'config'),patch.object(m,'STATE',Path(temp)/'state'):
                m.write_private(m.CONFIG,m.merge_target(self.a,self.b))
                retained={'fingerprint':m.fingerprint(self.b),'paused':True,'next_run':1234}
                m.write_private(m.STATE,{'targets':{m.target_key(self.a):{'paused':True},m.target_key(self.b):retained}})
                m.save_target(dict(self.a,token='c'*64))
                result=json.loads(m.STATE.read_text())['targets']
                self.assertEqual(result[m.target_key(self.b)],retained)
                self.assertEqual(result[m.target_key(self.a)],{})
                self.assertEqual(m.CONFIG.stat().st_mode&0o777,0o600)
    def test_missing_server_id_cannot_replace_multiple_scoped_targets(self):
        other=dict(self.a,server_id=2,token='c'*64)
        document=m.merge_target(self.a,other)
        ambiguous=dict(self.a);ambiguous.pop('server_id')
        with self.assertRaisesRegex(m.NetworkError,'specify --server-id'):
            m.merge_target(document,ambiguous)
        self.assertEqual(m.targets(document),[self.a,other])
    def test_duplicate_and_invalid_server_identity_rejected(self):
        with self.assertRaises(m.NetworkError):m.targets({'mode':'multi','targets':[self.a,self.a]})
        with self.assertRaises(m.NetworkError):m.validate_report(dict(self.a,server_id=True))

if __name__=='__main__':unittest.main()
