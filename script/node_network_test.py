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

if __name__=='__main__':unittest.main()
