"""Run actual lifecycle shell scripts in a temporary filesystem with fake services/releases."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
import zipfile
import shlex
import signal
import time
import textwrap


@unittest.skipUnless(os.name == 'posix' and shutil.which('bash'), 'Linux test environment with bash required')
class InstallerTypeTest(unittest.TestCase):
    def test_retired_node_type_is_rejected_before_installation(self):
        installer = Path(__file__).with_name('install.sh')
        result = subprocess.run(['bash', str(installer), '--node-type', 'v2node'], capture_output=True, text=True, timeout=5)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Invalid node type', result.stderr)


@unittest.skipUnless(os.name == 'posix' and shutil.which('jq'), 'Linux test environment with jq required')
class LifecycleTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.lib = self.root / 'usr/local/lib/v2box'
        self.lib.mkdir(parents=True)
        (self.root / 'usr/local/bin').mkdir(parents=True)
        self.config = self.root / 'etc/v2box'
        self.config.mkdir(parents=True)
        (self.config / 'config.json').write_text(json.dumps({'Nodes': [{'ApiHost': 'https://panel.example.test', 'ApiKey': 'secret', 'NodeID': 1}]}))
        (self.config / 'saved.cer').write_text('retain certificate')
        self.original_config = (self.config / 'config.json').read_bytes()
        unit = self.root / 'etc/systemd/system/v2box.service'
        unit.parent.mkdir(parents=True)
        unit.write_text('existing customized unit')
        (self.root/'run/systemd/system').mkdir(parents=True)
        self.active = self.root / 'active'
        self.active.touch()
        self.env = dict(os.environ, PATH=str(self.bin)+':'+str(self.root/'usr/local/bin')+':'+os.environ['PATH'], TEST_ROOT=str(self.root))
        self.write(self.lib/'v2box', '#!/bin/sh\necho old-version\n')
        self.installer = self.root/'install.sh'
        for name, target in [('install.sh', self.installer), ('v2box.sh', self.root/'usr/local/bin/v2box')]:
            source = Path(__file__).with_name(name).read_text()
            # All mutating absolute paths below are redirected into the sandbox;
            # allow this copy to exercise root-only branches as an ordinary CI user.
            source = source.replace('$EUID', '0')
            for prefix in ['/etc/v2box', '/etc/node-network', '/etc/systemd/system', '/etc/init.d', '/usr/local', '/var/log', '/proc/', '/run/v2box.pid', '/run/systemd/system']:
                source = source.replace(prefix, str(self.root)+prefix)
            self.write(target, source)
        self.write(self.bin/'sleep', '#!/bin/sh\nexit 0\n')
        self.write(self.bin/'systemctl', '''#!/bin/sh
echo "$*" >> "$TEST_ROOT/service-calls"
case "$1" in
 show) if test -f "$TEST_ROOT/mainpid"; then cat "$TEST_ROOT/mainpid"; else echo 0; fi;;
 is-active)
   if test -e "$TEST_ROOT/slow-polls"; then
     count=$(cat "$TEST_ROOT/slow-polls"); echo $((count+1)) > "$TEST_ROOT/slow-polls"
     if test "$count" -lt 2; then exit 3; fi
   fi
   test -e "$TEST_ROOT/active" || exit 3;;
 start|restart)
   if test -e "$TEST_ROOT/fail-start" && grep -q new-version "$TEST_ROOT/usr/local/lib/v2box/v2box"; then rm -f "$TEST_ROOT/active"; exit 1; fi
   touch "$TEST_ROOT/active"
   if test -e "$TEST_ROOT/slow-start"; then echo 0 > "$TEST_ROOT/slow-polls"; fi;;
 stop|disable) if test -e "$TEST_ROOT/fail-stop"; then exit 1; fi; rm -f "$TEST_ROOT/active";;
esac
''')
        self.write(self.bin/'curl', '''#!/bin/sh
out=''
while test "$#" -gt 0; do
 case "$1" in
  -o) out=$2; shift;;
  https:*) url=$1;;
 esac
 shift
done
case "$url" in
 */releases/latest) if test -n "$out"; then printf '{"tag_name":"v-test"}' > "$out"; else printf '{"tag_name":"v-test"}'; fi;;
 *.sha256) cp "$TEST_ROOT/release.zip.sha256" "$out";;
 *) cp "$TEST_ROOT/release.zip" "$out";;
esac
''')
        self.package()

    def write(self, path, value):
        path.write_text(value)
        path.chmod(0o755)

    def test_status_does_not_page_or_wait_for_deployment_lock(self):
        (self.config/'.deployment-pending').touch()
        self.write(self.bin/'systemctl', '#!/bin/sh\necho "$*"\nexit 3\n')
        result=subprocess.run(['bash',str(self.root/'usr/local/bin/v2box'),'status'],env=self.env,capture_output=True,text=True,timeout=5)
        self.assertEqual(result.returncode,3)
        self.assertIn('--no-pager -l',result.stdout)
        self.assertIn('Deployment is pending',result.stdout)

    def test_inactive_log_returns_and_explains_pending_deployment(self):
        self.active.unlink()
        (self.config/'.deployment-pending').touch()
        self.write(self.bin/'journalctl', '#!/bin/sh\nprintf "%s\\n" "$*"\n')
        result=subprocess.run(['bash',str(self.root/'usr/local/bin/v2box'),'log'],env=self.env,capture_output=True,text=True,timeout=5)
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertIn('without waiting',result.stdout)
        self.assertIn('deploy-log',result.stdout)
        self.assertNotIn(' -f',result.stdout)

    def test_active_log_follows_correct_service(self):
        self.write(self.bin/'journalctl', '#!/bin/sh\necho "$*"\n')
        for command,unit in [('log','v2box'),('deploy-log','v2box-deployment')]:
            result=subprocess.run(['bash',str(self.root/'usr/local/bin/v2box'),command],env=self.env,capture_output=True,text=True,timeout=5)
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertIn('-u '+unit+' -n 100 --no-pager -f',result.stdout)
            self.assertIn('Ctrl+C',result.stdout)

    def test_reload_verifies_running_capabilities_without_restart(self):
        process=self.root/'proc/123';process.mkdir(parents=True)
        self.write(process/'exe',"#!/bin/sh\necho '{\"features\":[\"multi-panel-v1\",\"reload-signal-v1\"]}'\n")
        (self.root/'mainpid').write_text('123')
        result=subprocess.run(['bash',str(self.root/'usr/local/bin/v2box'),'reload'],env=self.env,capture_output=True,text=True)
        self.assertEqual(result.returncode,0,result.stderr)
        calls=(self.root/'service-calls').read_text()
        self.assertIn('kill --kill-whom=main --signal=HUP v2box',calls);self.assertNotIn('restart',calls)
        self.assertTrue(self.active.exists());self.assertEqual((self.config/'config.json').read_bytes(),self.original_config)
    def test_shared_listener_rejects_old_program_without_changing_config(self):
        result = subprocess.run(['bash', str(self.root/'usr/local/bin/v2box'), 'share-listener', 'public', '1'], env=self.env, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Upgrade V2Box', result.stderr)
        self.assertEqual((self.config/'config.json').read_bytes(), self.original_config)
        self.assertEqual((self.config/'saved.cer').read_text(), 'retain certificate')

    def test_shared_listener_checks_running_program_before_writing(self):
        self.write(self.lib/'v2box', "#!/bin/sh\necho '" + json.dumps({'features': ['multi-panel-v1', 'reload-signal-v1', 'shared-listener-v1']}) + "'\n")
        process = self.root/'proc/123'
        process.mkdir(parents=True)
        self.write(process/'exe', "#!/bin/sh\necho '" + json.dumps({'features': ['multi-panel-v1', 'reload-signal-v1']}) + "'\n")
        (self.root/'mainpid').write_text('123')
        result = subprocess.run(['bash', str(self.root/'usr/local/bin/v2box'), 'share-listener', 'public', '1'], env=self.env, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.config/'config.json').read_bytes(), self.original_config)

    def test_shared_listener_supported_program_preserves_stopped_service(self):
        capabilities = json.dumps({'features': ['multi-panel-v1', 'reload-signal-v1', 'shared-listener-v1']})
        self.write(self.lib/'v2box', "#!/bin/sh\necho '" + capabilities + "'\n")
        source = Path(__file__).with_name('node-credentials.py').read_text()
        source = source.replace('"/etc/v2box/config.json"', repr(str(self.config/'config.json')))
        # Match the shell fixture's simulated root identity only in this private copy.
        source = source.replace('if os.geteuid() != 0:', 'if False:')
        self.write(self.lib/'node-credentials.py', source)
        self.active.unlink()
        result = subprocess.run(['bash', str(self.root/'usr/local/bin/v2box'), 'share-listener', 'public', '1'], env=self.env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        expected = json.loads(self.original_config)
        expected['Nodes'][0]['SharedListener'] = 'public'
        self.assertEqual(json.loads((self.config/'config.json').read_text()), expected)
        self.assertFalse(self.active.exists())
        self.assertNotIn('restart', (self.root/'service-calls').read_text())
        self.assertEqual((self.config/'saved.cer').read_text(), 'retain certificate')

    def test_reload_rejects_unsupported_running_binary(self):
        process=self.root/'proc/123';process.mkdir(parents=True)
        self.write(process/'exe','#!/bin/sh\necho old-version\n');(self.root/'mainpid').write_text('123')
        result=subprocess.run(['bash',str(self.root/'usr/local/bin/v2box'),'reload'],env=self.env,capture_output=True,text=True)
        self.assertNotEqual(result.returncode,0);self.assertIn('lacks independent',result.stderr)
        self.assertNotIn('kill ',(self.root/'service-calls').read_text());self.assertTrue(self.active.exists())
    def test_runtime_probe_rejects_invalid_process_id(self):
        (self.root/'mainpid').write_text('not-a-pid')
        result=subprocess.run(['bash',str(self.root/'usr/local/bin/v2box'),'check-runtime'],env=self.env,capture_output=True,text=True)
        self.assertNotEqual(result.returncode,0);self.assertNotIn('kill ',(self.root/'service-calls').read_text())

    def package(self, missing=None, overrides=None):
        files = {'v2box': '#!/bin/sh\necho new-version\n',
                 'install.sh': self.installer.read_text(),
                 'v2box.sh': (self.root/'usr/local/bin/v2box').read_text(),
                 'node-network.py': 'print("isolated IP helper")',
                 'node-credentials.py': '# helper', 'config.example.json': '{}'}
        files.update({name: 'fixture' for name in ['LICENSE', 'LICENSE.sing-box', 'NOTICE.md', 'README.md']})
        files.update(overrides or {})
        with zipfile.ZipFile(self.root/'release.zip', 'w') as archive:
            for name, value in files.items():
                if name != missing: archive.writestr(name, value)
        digest = hashlib.sha256((self.root/'release.zip').read_bytes()).hexdigest()
        (self.root/'release.zip.sha256').write_text(digest+'  v2box-linux-amd64.zip\n')

    def run_script(self, path, *args):
        return subprocess.run(['bash', str(path), *args], env=self.env, text=True, capture_output=True, timeout=20)

    def install(self, *args):
        return self.run_script(self.installer, '--update', *args)

    def preserved(self):
        self.assertEqual((self.config/'config.json').read_bytes(), self.original_config)
        self.assertEqual((self.config/'saved.cer').read_text(), 'retain certificate')

    @unittest.skipIf(os.path.exists('/dev/tty') and os.isatty(0), 'Requires noninteractive test process')
    def test_noninteractive_uninstall_requires_explicit_confirmation(self):
        result=self.run_script(self.root/'usr/local/bin/v2box','uninstall')
        self.assertNotEqual(result.returncode,0)
        self.assertIn('Nothing was removed',result.stderr)
        self.assertTrue((self.lib/'v2box').exists())
        self.assertTrue(self.active.exists())
        self.preserved()

    def test_missing_update_installer_has_repair_guidance(self):
        result=self.run_script(self.root/'usr/local/bin/v2box','update')
        self.assertNotEqual(result.returncode,0)
        self.assertIn('Update installer is missing',result.stderr)
        self.preserved()
        self.assertTrue(self.active.exists())

    def test_missing_reporter_uninstall_stops_schedule_directly(self):
        units=self.root/'etc/systemd/system'
        (units/'node-network.timer').write_text('fixture')
        (units/'node-network.service').write_text('fixture')
        result=self.run_script(self.root/'usr/local/bin/v2box','uninstall','--yes','--stop-network')
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertIn('helper is missing',result.stdout)
        calls=(self.root/'service-calls').read_text()
        self.assertIn('disable --now node-network.timer',calls)
        self.assertIn('stop node-network.service',calls)
        self.preserved()
        self.assertFalse((self.lib/'v2box').exists())

    def test_update_preserves_settings_and_supports_next_update(self):
        r = self.install()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn('new-version', (self.lib/'v2box').read_text())
        self.assertIn('old-version', (self.lib/'v2box.previous').read_text())
        self.assertEqual((self.root/'etc/systemd/system/v2box.service').read_text(), 'existing customized unit')
        r = self.run_script(self.root/'usr/local/bin/v2box', 'update', 'v-test')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.preserved()

    def test_failed_restart_rolls_back_program_and_service(self):
        (self.root/'fail-start').touch()
        r = self.install()
        self.assertNotEqual(r.returncode, 0)
        self.assertIn('restoring the previous', r.stderr)
        self.assertIn('old-version', (self.lib/'v2box').read_text())
        self.assertTrue(self.active.exists())
        self.preserved()

    def test_slow_start_is_retried_before_rollback(self):
        (self.root/'slow-start').touch()
        r=self.install()
        self.assertEqual(r.returncode,0,r.stderr)
        self.assertGreaterEqual(int((self.root/'slow-polls').read_text()),5)
        self.assertIn('new-version',(self.lib/'v2box').read_text())
        self.preserved()

    def test_invalid_release_scripts_preserve_existing_installation(self):
        for name in ['v2box.sh','install.sh']:
            self.package(overrides={name:'#!/bin/bash\nif then\n'})
            r=self.install()
            self.assertNotEqual(r.returncode,0)
            self.assertIn('invalid shell script',r.stderr)
            self.assertIn('old-version',(self.lib/'v2box').read_text())
            self.preserved()

    def test_invalid_python_helper_is_rejected_before_install(self):
        self.package(overrides={'node-network.py':'def broken(\n'})
        r=self.install()
        self.assertNotEqual(r.returncode,0)
        self.assertIn('invalid Python helper',r.stderr)
        self.assertIn('old-version',(self.lib/'v2box').read_text())
        self.preserved()

    def test_interrupted_download_retains_files_and_releases_locks(self):
        self.write(self.bin/'curl', '#!/bin/sh\ntouch "$TEST_ROOT/download-started"\nexec /bin/sleep 30\n')
        tmp=self.root/'tmp';tmp.mkdir();self.env['TMPDIR']=str(tmp)
        process=subprocess.Popen(['bash',str(self.installer),'--update','v-test'],env=self.env,text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
        try:
            deadline=time.monotonic()+3
            while not (self.root/'download-started').exists() and time.monotonic()<deadline: time.sleep(.02)
            self.assertTrue((self.root/'download-started').exists())
            os.killpg(process.pid,signal.SIGTERM)
            _,error=process.communicate(timeout=5)
            self.assertNotEqual(process.returncode,0)
            self.assertIn('rerun the panel',error)
            self.assertIn('old-version',(self.lib/'v2box').read_text())
            self.assertEqual(list(tmp.iterdir()),[])
            self.preserved()
            import fcntl
            for name in ['.bootstrap.lock','.deployment.lock','.maintenance.lock']:
                with (self.config/name).open('w') as lock: fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        finally:
            if process.poll() is None:
                os.killpg(process.pid,signal.SIGKILL);process.communicate()

    def test_unknown_service_state_never_replaces_or_removes_programs(self):
        self.write(self.bin/'timeout','''#!/bin/sh
if test "$4" = systemctl && test "$5" = is-active; then exit 124; fi
shift 3
exec "$@"
''')
        for r in [self.install(),self.run_script(self.root/'usr/local/bin/v2box','uninstall','--yes')]:
            self.assertNotEqual(r.returncode,0)
            self.assertIn('timed out',r.stderr)
            self.assertIn('old-version',(self.lib/'v2box').read_text())
            self.preserved()

    def test_uninstall_stop_timeout_retains_programs_and_service(self):
        self.write(self.bin/'timeout','''#!/bin/sh
if test "$4" = systemctl && test "$5" = disable; then exit 124; fi
shift 3
exec "$@"
''')
        r=self.run_script(self.root/'usr/local/bin/v2box','uninstall','--yes')
        self.assertNotEqual(r.returncode,0)
        self.assertIn('timed out',r.stderr)
        self.assertTrue((self.lib/'v2box').exists())
        self.assertTrue((self.root/'etc/systemd/system/v2box.service').exists())
        self.preserved()

    def test_binary_version_timeout_never_installs_the_release(self):
        self.write(self.bin/'timeout','''#!/bin/sh
if test "$3" = 15; then exit 124; fi
shift 3
exec "$@"
''')
        r=self.install()
        self.assertNotEqual(r.returncode,0)
        self.assertIn('version check',r.stderr)
        self.assertIn('old-version',(self.lib/'v2box').read_text())
        self.preserved()

    def test_failed_rollback_retains_backup_for_manual_recovery(self):
        (self.root/'fail-start').touch()
        tmp=self.root/'tmp';tmp.mkdir();self.env['TMPDIR']=str(tmp)
        self.write(self.bin/'cp','''#!/bin/sh
case "$3" in */previous/v2box) exit 1;; esac
exec /bin/cp "$@"
''')
        r=self.install()
        self.assertNotEqual(r.returncode,0)
        self.assertIn('Rollback incomplete',r.stderr)
        backup=next(line.split('Backup retained at ',1)[1] for line in r.stderr.splitlines() if 'Backup retained at ' in line)
        self.assertIn('old-version',(Path(backup)/'previous/v2box').read_text())
        self.preserved()

    def test_stopped_node_stays_stopped(self):
        self.active.unlink()
        r = self.install()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(self.active.exists())
        self.assertNotIn('restart', (self.root/'service-calls').read_text())

    def test_bad_release_and_checksum_do_not_replace_binary(self):
        for missing in ['v2box.sh', 'node-network.py', 'node-credentials.py', None]:
            self.package(missing)
            if missing is None: (self.root/'release.zip.sha256').write_text('0'*64+'  v2box-linux-amd64.zip\n')
            r = self.install()
            self.assertNotEqual(r.returncode, 0)
            self.assertIn('old-version', (self.lib/'v2box').read_text())
            self.preserved()

    def test_pending_update_preserves_state_and_stopped_service(self):
        state = self.config/'deployment-state.json'
        state.write_text('pending')
        self.active.unlink()
        r = self.install()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn('resume', r.stdout)
        self.assertEqual(state.read_text(), 'pending')
        self.assertFalse(self.active.exists())
        self.assertTrue((self.lib/'manager.sh').exists())
        r = self.run_script(self.root/'usr/local/bin/v2box', 'uninstall', '--yes')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(state.read_text(), 'pending')
        self.preserved()

    def test_resume_uses_retained_state(self):
        (self.lib/'node-network.py').write_text('import json; print(json.dumps({"features":["report-multi-target-v1"]}))')
        (self.config/'deployment-state.json').write_text('pending')
        (self.lib/'panel-deploy.py').write_text('''import json,sys
from pathlib import Path
if sys.argv[1]=='--capabilities':print(json.dumps({'features':['management-lock-delegation-v1']}))
else:
 assert sys.argv[1]=='--managed'
 assert Path(sys.argv[2]).read_text()=='pending'
''')
        r = self.run_script(self.root/'usr/local/bin/v2box', 'resume')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.preserved()

    def test_resume_rejects_retained_helper_without_lock_delegation(self):
        state=self.config/'deployment-state.json';state.write_text('pending')
        (self.lib/'panel-deploy.py').write_text('''import json,sys
def launch_managed(state):raise AssertionError('Old helper must not be launched')
if sys.argv[1]=='--capabilities':print(json.dumps({'features':[]}))
else:raise AssertionError('Deployment state must not be read')
''')
        result=self.run_script(self.root/'usr/local/bin/v2box','resume')
        self.assertNotEqual(result.returncode,0)
        self.assertIn('management lock delegation',result.stderr)
        self.assertIn('re-download',result.stderr);self.assertIn('--resume',result.stderr)
        self.assertEqual(state.read_text(),'pending')
        self.assertFalse((self.root/'service-calls').exists());self.preserved()

    def test_repair_missing_program_preserves_pending_state_without_start(self):
        (self.config/'deployment-state.json').write_text('pending')
        (self.lib/'v2box').unlink()
        self.active.unlink()
        r = self.run_script(self.installer)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(self.active.exists())
        self.assertEqual((self.config/'deployment-state.json').read_text(), 'pending')
        self.preserved()

    def test_active_deployment_blocks_update_and_uninstall(self):
        import fcntl
        with (self.config/'.deployment.lock').open('w') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            self.assertNotEqual(self.install().returncode, 0)
            self.assertNotEqual(self.run_script(self.root/'usr/local/bin/v2box', 'uninstall', '--yes').returncode, 0)
        self.assertTrue((self.lib/'v2box').exists())
        self.preserved()

    def test_symlinked_lock_is_rejected(self):
        (self.config/'.maintenance.lock').symlink_to(self.config/'saved.cer')
        self.assertNotEqual(self.install().returncode, 0)
        self.preserved()

    def test_bootstrap_delegates_its_lock_and_other_deployments_are_rejected(self):
        import fcntl
        lock_path = self.config/'.bootstrap.lock'
        with lock_path.open('w') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            r = self.install()
            self.assertNotEqual(r.returncode, 0)
            r = self.run_script(self.root/'usr/local/bin/v2box', 'uninstall', '--yes')
            self.assertNotEqual(r.returncode, 0)
            self.assertTrue((self.lib/'v2box').exists())
        command = f'exec 9>{shlex.quote(str(lock_path))}; flock -n 9; V2BOX_BOOTSTRAP_LOCK_HELD=1 bash {shlex.quote(str(self.installer))} --update 9>&-'
        r = subprocess.run(['bash', '-c', command], env=self.env, capture_output=True, text=True, timeout=20)
        self.assertEqual(r.returncode, 0, r.stderr)

    def test_update_and_uninstall_support_openrc(self):
        (self.bin/'systemctl').unlink()
        (self.root/'run/systemd/system').rmdir()
        # Alpine/CI test environment must not provide a real systemd daemon.
        self.write(self.bin/'rc-service', '''#!/bin/sh
case "$2" in
 status) test -e "$TEST_ROOT/active" || exit 3;;
 restart) touch "$TEST_ROOT/active";;
 stop) rm -f "$TEST_ROOT/active";;
esac
''')
        self.write(self.bin/'rc-update', '#!/bin/sh\nif test "$1" = show; then echo "v2box | default"; fi\n')
        init = self.root/'etc/init.d'
        init.mkdir()
        (init/'v2box').write_text('custom openrc service')
        # A systemctl executable on an OpenRC host is not an active systemd init.
        self.env['PATH'] = str(self.bin)+':'+str(self.root/'usr/local/bin')+':/usr/local/bin:/usr/bin:/bin'
        r = self.install()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual((init/'v2box').read_text(), 'custom openrc service')
        r = self.run_script(self.root/'usr/local/bin/v2box', 'uninstall', '--yes')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(self.active.exists())
        self.assertFalse((init/'v2box').exists())
        self.preserved()

    def test_mutating_commands_cannot_race_deployment_or_maintenance(self):
        import fcntl
        for name in ['.bootstrap.lock','.deployment.lock','.maintenance.lock']:
            with (self.config/name).open('w') as lock:
                fcntl.flock(lock,fcntl.LOCK_EX)
                for args in [('start',),('stop',),('restart',),('reload',),('enable',),('disable',),('credential',),('share-listener','group','1')]:
                    result=self.run_script(self.root/'usr/local/bin/v2box',*args)
                    self.assertNotEqual(result.returncode,0,(name,args,result.stdout,result.stderr))
            self.assertTrue(self.active.exists())
            self.preserved()

    def test_panel_delegates_only_its_locks_and_maintenance_remains_exclusive(self):
        import fcntl
        self.env.update(V2BOX_BOOTSTRAP_LOCK_HELD='1',V2BOX_DEPLOYMENT_LOCK_HELD='1')
        with (self.config/'.bootstrap.lock').open('w') as bootstrap,(self.config/'.deployment.lock').open('w') as deployment:
            fcntl.flock(bootstrap,fcntl.LOCK_EX);fcntl.flock(deployment,fcntl.LOCK_EX)
            result=self.run_script(self.root/'usr/local/bin/v2box','restart')
            self.assertEqual(result.returncode,0,result.stderr)
            with (self.config/'.maintenance.lock').open('w') as maintenance:
                fcntl.flock(maintenance,fcntl.LOCK_EX)
                result=self.run_script(self.root/'usr/local/bin/v2box','stop')
                self.assertNotEqual(result.returncode,0)
        self.assertTrue(self.active.exists());self.preserved()

    def test_successful_service_command_does_not_hide_dead_start_or_failed_stop(self):
        self.write(self.bin/'systemctl','''#!/bin/sh
echo "$*" >> "$TEST_ROOT/service-calls"
case "$1" in is-active) test -e "$TEST_ROOT/active" || exit 3;; esac
''')
        self.active.unlink()
        for action in ['start','restart']:
            result=self.run_script(self.root/'usr/local/bin/v2box',action)
            self.assertNotEqual(result.returncode,0)
            self.assertIn('did not remain active',result.stderr)
        self.active.touch()
        result=self.run_script(self.root/'usr/local/bin/v2box','stop')
        self.assertNotEqual(result.returncode,0)
        self.assertIn('did not remain stopped',result.stderr)
        self.assertTrue(self.active.exists());self.preserved()

    def test_archive_timeout_does_not_publish_or_keep_locks(self):
        self.write(self.bin/'timeout','''#!/bin/sh
if test "$4" = unzip; then exit 124; fi
shift 3
exec "$@"
''')
        result=self.install()
        self.assertNotEqual(result.returncode,0)
        self.assertIn('step timed out',result.stderr)
        self.assertIn('old-version',(self.lib/'v2box').read_text())
        self.preserved()
        import fcntl
        for name in ['.bootstrap.lock','.deployment.lock','.maintenance.lock']:
            with (self.config/name).open('w') as lock:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)

    def test_version_child_cannot_inherit_installer_locks(self):
        self.package(overrides={'v2box':'''#!/bin/sh
if test "$1" = version; then
 /bin/sleep 30 >/dev/null 2>&1 &
 echo $! > "$TEST_ROOT/version-child"
fi
echo new-version
'''})
        try:
            result=self.install()
            self.assertEqual(result.returncode,0,result.stderr)
            pid=int((self.root/'version-child').read_text())
            os.kill(pid,0)
            import fcntl
            for name in ['.bootstrap.lock','.deployment.lock','.maintenance.lock']:
                with (self.config/name).open('w') as lock:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            self.preserved()
        finally:
            if (self.root/'version-child').exists():
                try:os.kill(int((self.root/'version-child').read_text()),signal.SIGTERM)
                except ProcessLookupError:pass

    def test_manager_interrupt_cleans_timeout_group_and_term_ignoring_grandchild(self):
        self.assert_interrupt_cleans_service_group()

    def test_manager_interrupt_does_not_require_timeout_to_escalate_term(self):
        self.stubborn_timeout()
        self.assert_interrupt_cleans_service_group()

    def test_manager_signal_between_spawn_and_pid_registration_cleans_group(self):
        self.stubborn_timeout()
        self.assert_interrupt_cleans_service_group(during_start=True)

    def test_installer_signal_between_spawn_and_pid_registration_cleans_group(self):
        self.stubborn_timeout()
        self.assert_interrupt_cleans_service_group(during_start=True,installer=True)

    def stubborn_timeout(self):
        # Emulate a timeout supervisor that owns its group but does not escalate
        # an external TERM. Cleanup must enforce its own deadline on every GNU
        # coreutils version, rather than relying on that signal-handling detail.
        self.write(self.bin/'timeout','''#!/usr/bin/env python3
import os,signal,subprocess,sys
if sys.argv[1]=='--foreground':os.execvp(sys.argv[3],sys.argv[3:])
os.setpgid(0,0)
signal.signal(signal.SIGTERM,signal.SIG_IGN)
sys.exit(subprocess.Popen(sys.argv[4:]).wait())
''')

    def assert_interrupt_cleans_service_group(self,during_start=False,installer=False):
        self.write(self.bin/'systemctl','''#!/bin/bash
trap '' TERM
echo "$PPID" > "$TEST_ROOT/service-timeout-pid"
/bin/bash -c 'trap "" TERM; echo $$ > "$TEST_ROOT/service-grandchild"; exec /bin/sleep 30' &
wait
''')
        script=self.installer if installer else self.root/'usr/local/bin/v2box'
        if during_start:
            source=script.read_text()
            source=source.replace('  child_pid=$!', '''  while [[ ! -f "$TEST_ROOT/service-grandchild" ]]; do /bin/sleep .01; done
  kill -TERM "$$"
  child_pid=$!''',1)
            self.write(script,source)
        arguments=['--update','v-test'] if installer else ['restart']
        process=subprocess.Popen(['bash',str(script),*arguments],env=self.env,text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
        try:
            deadline=time.monotonic()+3
            while not (self.root/'service-grandchild').exists() and time.monotonic()<deadline:time.sleep(.02)
            self.assertTrue((self.root/'service-grandchild').exists())
            if not during_start:process.send_signal(signal.SIGTERM)
            process.communicate(timeout=8)
            self.assertNotEqual(process.returncode,0)
            for name in ['service-grandchild','service-timeout-pid']:
                pid=int((self.root/name).read_text())
                stat=Path('/proc')/str(pid)/'stat'
                self.assertTrue(not stat.exists() or stat.read_text().split()[2]=='Z','A service process must not survive or remain stopped after the interruption deadline')
            import fcntl
            for name in ['.bootstrap.lock','.deployment.lock','.maintenance.lock']:
                with (self.config/name).open('w') as lock:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            self.preserved()
        finally:
            if (self.root/'service-timeout-pid').exists():
                try:os.killpg(int((self.root/'service-timeout-pid').read_text()),signal.SIGKILL)
                except ProcessLookupError:pass
            if process.poll() is None:os.killpg(process.pid,signal.SIGKILL)
            process.communicate(timeout=5)

    def test_interrupt_after_publication_restores_program_and_running_service(self):
        self.assert_interrupt_after_publication()

    def test_signal_during_restart_spawn_restores_program_and_running_service(self):
        self.assert_interrupt_after_publication(during_start=True)

    def assert_interrupt_after_publication(self,during_start=False):
        self.write(self.bin/'systemctl','''#!/bin/sh
echo "$*" >> "$TEST_ROOT/service-calls"
case "$1" in
 is-active) test -e "$TEST_ROOT/active" || exit 3;;
 restart)
  if grep -q new-version "$TEST_ROOT/usr/local/lib/v2box/v2box"; then
   rm -f "$TEST_ROOT/active";touch "$TEST_ROOT/restart-started";exec /bin/sleep 30
  fi
  touch "$TEST_ROOT/active";;
esac
''')
        if during_start:
            source=self.installer.read_text().replace('  child_pid=$!', '''  if [[ "$1" == systemctl && "${2:-}" == restart && ! -f "$TEST_ROOT/window-signalled" ]]; then
    while [[ ! -f "$TEST_ROOT/restart-started" ]]; do /bin/sleep .01; done
    touch "$TEST_ROOT/window-signalled"
    kill -TERM "$$"
  fi
  child_pid=$!''',1)
            self.write(self.installer,source)
        process=subprocess.Popen(['bash',str(self.installer),'--update','v-test'],env=self.env,text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
        try:
            deadline=time.monotonic()+5
            while not (self.root/'restart-started').exists() and time.monotonic()<deadline:time.sleep(.02)
            self.assertTrue((self.root/'restart-started').exists())
            if not during_start:process.send_signal(signal.SIGTERM)
            _,error=process.communicate(timeout=8)
            self.assertNotEqual(process.returncode,0)
            self.assertIn('restoring the previous',error)
            self.assertIn('old-version',(self.lib/'v2box').read_text())
            self.assertTrue(self.active.exists())
            self.preserved()
        finally:
            if process.poll() is None:os.killpg(process.pid,signal.SIGKILL);process.communicate()

    def test_non_gnu_timeout_is_rejected_before_mutation(self):
        self.write(self.bin/'timeout','#!/bin/sh\necho unsupported >&2\nexit 2\n')
        for result in [self.install(),self.run_script(self.root/'usr/local/bin/v2box','restart')]:
            self.assertNotEqual(result.returncode,0)
            self.assertIn('GNU coreutils timeout',result.stderr)
            self.assertIn('old-version',(self.lib/'v2box').read_text())
            self.assertTrue(self.active.exists());self.preserved()

    def test_example_or_incomplete_active_configuration_never_starts_service(self):
        original=json.loads(self.original_config)
        for override in [{'ApiKey':'REPLACE_ME'},{'ApiKey':'  replace_me  '},{'ApiKey':' '},{'ApiHost':'https://panel.example.com'},{'NodeID':0},{'NodeID':1.5}]:
            document=json.loads(self.original_config);document['Nodes'][0].update(override)
            (self.config/'config.json').write_text(json.dumps(document))
            for action in ['start','restart']:
                result=self.run_script(self.root/'usr/local/bin/v2box',action)
                self.assertNotEqual(result.returncode,0)
                self.assertIn('resume',result.stderr)
                self.assertFalse((self.root/'service-calls').exists())
                self.assertEqual(json.loads((self.config/'config.json').read_text()),document)
        (self.config/'config.json').write_bytes(self.original_config)
        (self.config/'deployment-state-new.json').write_text('new deployment pending')
        result=self.run_script(self.root/'usr/local/bin/v2box','restart')
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertTrue(self.active.exists());self.preserved()

    def test_partial_program_publication_is_removed_after_failed_update(self):
        real_install=shutil.which('install')
        for suffix in ['v2box.new','manager.sh.new','install.sh.new','node-credentials.py.new','node-network.py.new']:
            self.write(self.bin/'install','#!/bin/bash\ncase "${@: -1}" in */'+suffix+') printf partial > "${@: -1}";exit 1;; esac\nexec '+shlex.quote(real_install)+' "$@"\n')
            result=self.install()
            self.assertNotEqual(result.returncode,0,(suffix,result.stdout,result.stderr))
            self.assertFalse((self.lib/suffix).exists())
            self.assertIn('old-version',(self.lib/'v2box').read_text())
            self.preserved()
        self.assertFalse((self.root/'usr/local/bin/v2box.new').exists())

    def test_uninstall_requires_valid_options_and_preserves_config(self):
        wrapper = self.root/'usr/local/bin/v2box'
        r = self.run_script(wrapper, 'uninstall', '--purge')
        self.assertNotEqual(r.returncode, 0)
        self.assertTrue((self.lib/'v2box').exists())
        r = self.run_script(wrapper, 'uninstall', '--yes')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(wrapper.exists())
        self.assertFalse((self.lib/'v2box').exists())
        self.assertFalse(self.active.exists())
        self.preserved()

    def test_failed_stop_never_removes_running_program(self):
        (self.root/'fail-stop').touch()
        r = self.run_script(self.root/'usr/local/bin/v2box', 'uninstall', '--yes')
        self.assertNotEqual(r.returncode, 0)
        self.assertTrue((self.lib/'v2box').exists())
        self.preserved()

    def test_uninstall_stops_reporter_only_when_explicitly_requested(self):
        cfg = self.root/'etc/node-network'
        cfg.mkdir()
        (cfg/'cloudflare.json').write_text('{}')
        helper = self.root/'usr/local/lib/node-network'
        helper.mkdir()
        (helper/'node-network.py').write_text('import os,sys\nfrom pathlib import Path\nassert sys.argv[1]=="disable"\nPath(os.environ["TEST_ROOT"],"reporter-stopped").touch()')
        r = self.run_script(self.root/'usr/local/bin/v2box', 'uninstall', '--yes', '--stop-network')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue((self.root/'reporter-stopped').exists())
        self.assertTrue((cfg/'cloudflare.json').exists())
        self.preserved()

    def test_install_does_not_probe_ip_before_report_configuration(self):
        self.package(overrides={'node-network.py': 'raise RuntimeError("unnecessary IP probe")'})
        r=self.install()
        self.assertEqual(r.returncode,0,r.stderr)
        self.assertNotIn('unnecessary IP probe',r.stderr)

    def test_fresh_install_creates_private_configuration_and_starts_with_explicit_identity(self):
        (self.config/'config.json').unlink();(self.lib/'v2box').unlink()
        (self.root/'etc/systemd/system/v2box.service').unlink();self.active.unlink()
        r=self.run_script(self.installer,'v-test','--api-host','https://fresh.example.test','--node-id','9','--api-key','fresh-fixture-key')
        self.assertEqual(r.returncode,0,r.stderr)
        document=json.loads((self.config/'config.json').read_text())
        self.assertEqual(document['Nodes'][0]['NodeID'],9);self.assertEqual(document['Nodes'][0]['ApiHost'],'https://fresh.example.test')
        self.assertEqual((self.config/'config.json').stat().st_mode&0o777,0o600)
        self.assertTrue(self.active.exists());self.assertIn('ExecStart=',(self.root/'etc/systemd/system/v2box.service').read_text())
        self.assertNotIn('fresh-fixture-key',r.stdout+r.stderr)
    def test_fresh_example_install_never_starts_unenrolled_service(self):
        (self.config/'config.json').unlink();(self.lib/'v2box').unlink()
        (self.root/'etc/systemd/system/v2box.service').unlink();self.active.unlink()
        self.package(overrides={'config.example.json':json.dumps({'Nodes':[{'ApiHost':'https://panel.example.com','ApiKey':'REPLACE_ME','NodeID':1}]})})
        r=self.run_script(self.installer,'v-test')
        self.assertEqual(r.returncode,0,r.stderr);self.assertFalse(self.active.exists())
        self.assertNotIn('restart',(self.root/'service-calls').read_text())
    def test_reinstall_arguments_never_replace_existing_multi_panel_configuration(self):
        document={'OperatorSetting':'retain','Nodes':[{'ApiHost':'https://a.example.test','NodeID':3,'ApiKey':'a-fixture-key'},{'ApiHost':'https://b.example.test','NodeID':3,'ApiKey':'b-fixture-key'}]}
        self.original_config=json.dumps(document).encode();(self.config/'config.json').write_bytes(self.original_config)
        r=self.run_script(self.installer,'v-test','--api-host','https://new.example.test','--node-id','4','--api-key','new-fixture-key')
        self.assertEqual(r.returncode,0,r.stderr);self.preserved()
    def test_uninstall_then_reinstall_preserves_multi_panel_certificates_guards_and_reporting(self):
        document={'OperatorSetting':'retain','Nodes':[{'ApiHost':'https://a.example.test','NodeID':3,'ApiKey':'a-fixture-key'},{'ApiHost':'https://b.example.test','NodeID':3,'ApiKey':'b-fixture-key'}]}
        self.original_config=json.dumps(document).encode();(self.config/'config.json').write_bytes(self.original_config)
        report=self.root/'etc/node-network';report.mkdir()
        (report/'cloudflare.json').write_text(json.dumps({'mode':'multi','targets':[{'mode':'panel','panel_url':'https://a.example.test','token':'a-report-fixture'},{'mode':'panel','panel_url':'https://b.example.test','token':'b-report-fixture'}]}))
        for panel in ('a','b'):
            cert=self.config/'panels'/panel;cert.mkdir(parents=True)
            for name in ('cert.pem','key.pem','acme-state.json','user.json'):(cert/name).write_text(panel+'-'+name)
        sentinel={p:p.read_bytes() for p in self.config.rglob('*') if p.is_file() and not p.name.startswith('.')}
        reports=(report/'cloudflare.json').read_bytes()
        for cycle in range(2):
            wrapper=self.root/'usr/local/bin/v2box'
            r=self.run_script(wrapper,'uninstall','--yes')
            self.assertEqual(r.returncode,0,r.stderr);self.assertFalse(self.active.exists());self.assertFalse((self.lib/'v2box').exists())
            self.assertEqual((report/'cloudflare.json').read_bytes(),reports)
            for path,value in sentinel.items():self.assertEqual(path.read_bytes(),value)
            refused=self.install();self.assertNotEqual(refused.returncode,0)
            r=self.run_script(self.installer,'v-test')
            self.assertEqual(r.returncode,0,r.stderr);self.assertTrue(self.active.exists())
            for path,value in sentinel.items():self.assertEqual(path.read_bytes(),value)
            self.assertEqual((report/'cloudflare.json').read_bytes(),reports);self.preserved()
    def test_failed_first_configuration_write_is_retryable(self):
        (self.config/'config.json').unlink()
        (self.root/'active').unlink()
        self.write(self.bin/'jq', '#!/bin/sh\nprintf "partial json"\nexit 1\n')
        r=self.run_script(self.installer,'v-test','--api-host','https://actual.example.test','--node-id','7','--api-key','new-secret')
        self.assertNotEqual(r.returncode,0)
        self.assertFalse((self.config/'config.json').exists())
        self.assertFalse((self.config/'config.json.new').exists())
        (self.bin/'jq').unlink()
        r=self.run_script(self.installer,'v-test','--api-host','https://actual.example.test','--node-id','7','--api-key','new-secret')
        self.assertEqual(r.returncode,0,r.stderr)
        self.assertEqual(json.loads((self.config/'config.json').read_text())['Nodes'][0]['NodeID'],7)
        self.assertEqual((self.config/'saved.cer').read_text(),'retain certificate')

    def test_failed_service_publication_is_retryable(self):
        unit=self.root/'etc/systemd/system/v2box.service'
        unit.unlink()
        self.active.unlink()
        real_install=shutil.which('install')
        self.write(self.bin/'install', '#!/bin/sh\ncase "$*" in *v2box.service.new*) printf partial > "${@: -1}"; exit 1;; esac\nexec '+shlex.quote(real_install)+' "$@"\n')
        # Use bash for the positional-argument fixture, matching the installer.
        (self.bin/'install').write_text((self.bin/'install').read_text().replace('#!/bin/sh','#!/bin/bash'))
        r=self.install()
        self.assertNotEqual(r.returncode,0)
        self.assertFalse(unit.exists())
        self.assertFalse(unit.with_name('v2box.service.new').exists())
        (self.bin/'install').unlink()
        r=self.install()
        self.assertEqual(r.returncode,0,r.stderr)
        self.assertIn('ExecStart=',unit.read_text())
        self.preserved()


@unittest.skipUnless(os.name == 'posix' and all(shutil.which(x) for x in ('zip', 'unzip', 'sha256sum')), 'Linux packaging tools required')
class ReleasePackageTest(unittest.TestCase):
    def package(self, platform='linux', missing=None):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        workspace = root/'workspace'
        workspace.mkdir()
        binaries = root/'bin'
        binaries.mkdir()
        scratch = root/'scratch'
        scratch.mkdir()
        required = ['README.md', 'LICENSE', 'LICENSE.sing-box', 'NOTICE.md', 'config.example.json']
        required += ['script/install.sh', 'script/v2box.sh', 'script/node-network.py', 'script/node-credentials.py', 'internal/licenses/license.txt']
        for name in required:
            if name == missing:
                continue
            target = workspace/name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text('fixture')
        old = workspace/'build_assets'
        old.mkdir()
        (old/'operator-private.log').write_text('must never be packaged')
        archive = workspace/f'v2box-{platform}-amd64.zip'
        with zipfile.ZipFile(archive, 'w') as stream:
            stream.writestr('stale.txt', 'must not survive archive replacement')
        original = archive.read_bytes()
        go = binaries/'go'
        go.write_text('#!/bin/sh\noutput=""\nwhile [ "$#" -gt 0 ]; do if [ "$1" = -o ]; then shift; output=$1; fi; shift; done\nprintf test-binary > "$output"\n')
        go.chmod(0o755)
        workflow = Path(__file__).resolve().parents[1]/'.github/workflows/release.yml'
        text = workflow.read_text()
        start = text.index('        run: |', text.index('      - name: Build')) + len('        run: |\n')
        end = text.index('      - uses: actions/upload-artifact', start)
        script = root/'build.sh'
        script.write_text(textwrap.dedent(text[start:end]))
        env = dict(os.environ, PATH=str(binaries)+':'+os.environ['PATH'], TMPDIR=str(scratch), GOOS=platform, GOARCH='amd64', GITHUB_REF_NAME='v-test')
        result = subprocess.run(['bash', '-e', '-o', 'pipefail', str(script)], cwd=workspace, env=env, capture_output=True, text=True, timeout=30)
        return result, archive, original, scratch

    def test_archives_exclude_old_outputs_and_replace_stale_entries(self):
        for platform in ('linux', 'windows'):
            with self.subTest(platform=platform):
                result, archive, _, scratch = self.package(platform)
                self.assertEqual(result.returncode, 0, result.stderr)
                with zipfile.ZipFile(archive) as stream:
                    names = set(stream.namelist())
                    self.assertNotIn('operator-private.log', names)
                    self.assertNotIn('stale.txt', names)
                    self.assertEqual(stream.read('v2box.exe' if platform == 'windows' else 'v2box'), b'test-binary')
                    self.assertIn('node-credentials.py', names)
                    self.assertIn('licenses/license.txt', names)
                self.assertEqual(archive.with_suffix('.zip.sha256').read_text().split()[0], hashlib.sha256(archive.read_bytes()).hexdigest())
                self.assertEqual(list(scratch.iterdir()), [])

    def test_missing_required_file_preserves_previous_archive_and_cleans_scratch(self):
        result, archive, original, scratch = self.package(missing='script/node-credentials.py')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(archive.read_bytes(), original)
        self.assertEqual(list(scratch.iterdir()), [])


if __name__ == '__main__':
    unittest.main()
