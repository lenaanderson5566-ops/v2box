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
            for prefix in ['/etc/v2box', '/etc/node-network', '/etc/systemd/system', '/etc/init.d', '/usr/local', '/var/log']:
                source = source.replace(prefix, str(self.root)+prefix)
            self.write(target, source)
        self.write(self.bin/'sleep', '#!/bin/sh\nexit 0\n')
        self.write(self.bin/'systemctl', '''#!/bin/sh
echo "$*" >> "$TEST_ROOT/service-calls"
case "$1" in
 is-active)
   if test -e "$TEST_ROOT/slow-polls"; then
     count=$(cat "$TEST_ROOT/slow-polls"); echo $((count+1)) > "$TEST_ROOT/slow-polls"
     if test "$count" -lt 2; then exit 3; fi
   fi
   test -e "$TEST_ROOT/active" || exit 3;;
 restart)
   if test -e "$TEST_ROOT/fail-start" && grep -q new-version "$TEST_ROOT/usr/local/lib/v2box/v2box"; then rm -f "$TEST_ROOT/active"; exit 1; fi
   touch "$TEST_ROOT/active"
   if test -e "$TEST_ROOT/slow-start"; then echo 0 > "$TEST_ROOT/slow-polls"; fi;;
 disable) if test -e "$TEST_ROOT/fail-stop"; then exit 1; fi; rm -f "$TEST_ROOT/active";;
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
 */releases/latest) printf '{"tag_name":"v-test"}';;
 *.sha256) cp "$TEST_ROOT/release.zip.sha256" "$out";;
 *) cp "$TEST_ROOT/release.zip" "$out";;
esac
''')
        self.package()

    def write(self, path, value):
        path.write_text(value)
        path.chmod(0o755)

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
        (self.config/'deployment-state.json').write_text('pending')
        (self.lib/'panel-deploy.py').write_text('import sys; from pathlib import Path; assert Path(sys.argv[1]).read_text() == "pending"')
        r = self.run_script(self.root/'usr/local/bin/v2box', 'resume')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.preserved()

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
        # Hide host systemctl without replacing production service selection.
        self.env['PATH'] = str(self.bin)+':'+str(self.root/'usr/local/bin')+':/usr/local/bin:/usr/bin:/bin'
        if shutil.which('systemctl', path=self.env['PATH']):
            self.skipTest('OpenRC test requires an environment without systemctl')
        r = self.install()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual((init/'v2box').read_text(), 'custom openrc service')
        r = self.run_script(self.root/'usr/local/bin/v2box', 'uninstall', '--yes')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(self.active.exists())
        self.assertFalse((init/'v2box').exists())
        self.preserved()

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


if __name__ == '__main__':
    unittest.main()
