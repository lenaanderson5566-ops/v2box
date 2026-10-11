#!/usr/bin/env python3
"""Public IP discovery and opt-in Cloudflare DDNS; no third-party Python packages."""
import argparse
import getpass
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import tempfile
import urllib.error
import urllib.parse
import urllib.request

CONFIG = Path('/etc/node-network/cloudflare.json')
STATE = Path('/var/lib/node-network/state.json')
INSTALLED = Path('/usr/local/lib/node-network/node-network.py')
IP_SERVICES = {
    'A': ['https://api4.ipify.org', 'https://ipv4.icanhazip.com'],
    'AAAA': ['https://api6.ipify.org', 'https://ipv6.icanhazip.com'],
}

class NetworkError(Exception):
    def __init__(self, message, permanent=False, retry_after=300):
        super().__init__(message)
        self.permanent = permanent
        self.retry_after = retry_after

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None

class Client:
    def request(self, url, method='GET', data=None, token=None):
        headers = {'User-Agent': 'node-network/1', 'Accept': 'application/json'}
        if token:
            headers['Authorization'] = 'Bearer ' + token
        if data is not None:
            headers['Content-Type'] = 'application/json'
        req = urllib.request.Request(url, data=None if data is None else json.dumps(data).encode(), headers=headers, method=method)
        try:
            with urllib.request.build_opener(NoRedirect()).open(req, timeout=10) as response:
                body = response.read(262145)
                if len(body) > 262144:
                    raise NetworkError('Response exceeded size limit')
                return body.decode('utf-8')
        except urllib.error.HTTPError as error:
            delay = error.headers.get('Retry-After', '')
            delay = min(max(int(delay), 300), 86400) if delay.isdigit() else 900
            raise NetworkError('HTTP request failed: %s' % error.code, error.code in (400, 401, 403, 404, 422), delay) from None
        except (OSError, UnicodeError):
            raise NetworkError('Network request failed') from None

    def cloudflare(self, config, path='', method='GET', data=None):
        raw = self.request('https://api.cloudflare.com/client/v4/zones/' + config['zone_id'] + '/dns_records' + path, method, data, config['token'])
        try:
            response = json.loads(raw)
            if not response.get('success'):
                raise NetworkError('Cloudflare rejected the operation; verify token, zone and record', permanent=True)
            return response
        except (ValueError, AttributeError):
            raise NetworkError('Invalid Cloudflare response') from None


def discover(client, record_type):
    for url in IP_SERVICES[record_type]:
        try:
            address = ipaddress.ip_address(client.request(url).strip())
            if address.version != (4 if record_type == 'A' else 6):
                continue
            if address.is_global and not address.is_multicast:
                return str(address)
        except (NetworkError, ValueError):
            pass
    raise NetworkError('No valid public %s address detected' % ('IPv4' if record_type == 'A' else 'IPv6'))


def validate(config):
    if not re.fullmatch(r'[a-fA-F0-9]{32}', config.get('zone_id', '')):
        raise NetworkError('Cloudflare Zone ID must be 32 hexadecimal characters', True)
    domain = config.get('domain', '').rstrip('.').lower()
    try:
        domain = domain.encode('idna').decode('ascii')
    except UnicodeError:
        raise NetworkError('Invalid domain', True) from None
    if len(domain) > 253 or '.' not in domain or not all(re.fullmatch(r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?', part) for part in domain.split('.')):
        raise NetworkError('Use a full DNS hostname, for example node.example.com', True)
    try:
        ipaddress.ip_address(domain)
    except ValueError:
        pass
    else:
        raise NetworkError('DDNS requires a hostname, not an IP address', True)
    if config.get('family') not in ('ipv4', 'ipv6', 'dual'):
        raise NetworkError('Invalid address family', True)
    token = config.get('token', '')
    if not isinstance(token, str) or not token or any(c.isspace() for c in token):
        raise NetworkError('Invalid Cloudflare API token', True)
    return dict(config, domain=domain)


def record_types(family):
    return ['A', 'AAAA'] if family == 'dual' else ['AAAA'] if family == 'ipv6' else ['A']


def sync(client, config):
    config = validate(config)
    failures = []
    for typ in record_types(config['family']):
        try:
            address = discover(client, typ)
        except NetworkError as error:
            failures.append(str(error))
            continue  # Never erase a DNS record when a family is temporarily unavailable.
        response = client.cloudflare(config, '?' + urllib.parse.urlencode({'type': typ, 'name': config['domain'], 'per_page': 100}))
        records = response.get('result')
        if not isinstance(records, list) or len(records) > 1 or response.get('result_info', {}).get('total_pages', 1) > 1:
            raise NetworkError('Ambiguous DNS records; choose a dedicated hostname', True)
        if records and (records[0].get('type') != typ or records[0].get('name') != config['domain']):
            raise NetworkError('Unexpected DNS record returned', True)
        data = {'type': typ, 'name': config['domain'], 'content': address, 'ttl': 300, 'proxied': False}
        if not records:
            client.cloudflare(config, method='POST', data=data)
            print('%s record created: %s' % (typ, address))
        elif records[0].get('content') == address and records[0].get('proxied') is False:
            print('%s unchanged: %s' % (typ, address))
        else:
            record_id = records[0].get('id', '')
            if not re.fullmatch(r'[a-fA-F0-9]{32}', record_id):
                raise NetworkError('Invalid DNS record ID returned', True)
            client.cloudflare(config, '/' + record_id, 'PATCH', data)
            print('%s record updated: %s' % (typ, address))
    if failures:
        raise NetworkError('; '.join(failures))


def validate_report(config):
    url = config.get('panel_url', '').rstrip('/')
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != 'https' or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in ('', '/'):
        raise NetworkError('Use the HTTPS panel origin, for example https://panel.example.com', True)
    if not re.fullmatch(r'[a-f0-9]{64}', config.get('token', '')):
        raise NetworkError('Invalid node report key', True)
    if config.get('family') not in ('ipv4', 'ipv6', 'dual'):
        raise NetworkError('Invalid address family', True)
    return dict(config, panel_url=url, mode='panel')


def report(client, config):
    config = validate_report(config)
    addresses = {}
    for typ in record_types(config['family']):
        try:
            addresses['ipv4' if typ == 'A' else 'ipv6'] = discover(client, typ)
        except NetworkError:
            pass  # Never overwrite a previously valid address with an empty value.
    if not addresses:
        raise NetworkError('No valid public address detected')
    raw = client.request(config['panel_url'] + '/api/v10/server/addresses', 'POST', addresses, config['token'])
    try:
        result = json.loads(raw)
        if not isinstance(result, dict) or result.get('data', {}).get('accepted') is not True:
            raise ValueError()
    except (ValueError, AttributeError):
        raise NetworkError('Invalid panel response') from None
    print('Public IP report accepted')


def execute(client, config):
    return report(client, config) if config.get('mode') == 'panel' else sync(client, config)


def write_private(path, data):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, name = tempfile.mkstemp(prefix=path.name+'.', dir=str(path.parent))
    temp = Path(name)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, 'w') as out:
            json.dump(data, out)
            out.flush()
            os.fsync(out.fileno())
        os.replace(temp, path)
    finally:
        temp.unlink(missing_ok=True)


def run():
    import fcntl
    STATE.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    with open(STATE.parent / 'lock', 'w') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        state = json.loads(STATE.read_text()) if STATE.exists() else {}
        if state.get('paused'):
            print('DDNS paused after configuration/authentication failure; reconfigure node-network to resume')
            return
        if time.time() < state.get('next_run', 0):
            return
        try:
            if CONFIG.stat().st_mode & 0o077:
                raise NetworkError('DDNS configuration must have mode 600', True)
            execute(Client(), json.loads(CONFIG.read_text()))
        except NetworkError as error:
            write_private(STATE, {'paused': error.permanent, 'next_run': time.time() + error.retry_after})
            raise
        write_private(STATE, {})


def schedule(enabled):
    if Path('/run/systemd/system').exists():
        if enabled:
            Path('/etc/systemd/system/node-network.service').write_text('[Unit]\nDescription=Cloudflare node DDNS\nWants=network-online.target\nAfter=network-online.target\n[Service]\nType=oneshot\nExecStart=/usr/bin/env python3 /usr/local/lib/node-network/node-network.py run\nUMask=0077\n')
            Path('/etc/systemd/system/node-network.timer').write_text('[Unit]\nDescription=Check node DNS every five minutes\n[Timer]\nOnBootSec=1min\nOnUnitActiveSec=5min\nRandomizedDelaySec=30s\n[Install]\nWantedBy=timers.target\n')
            subprocess.run(['systemctl', 'daemon-reload'], check=True)
        subprocess.run(['systemctl', 'enable' if enabled else 'disable', '--now', 'node-network.timer'], check=True)
    elif shutil.which('crontab') and shutil.which('rc-service'):
        result = subprocess.run(['crontab', '-l'], capture_output=True, text=True)
        if result.returncode and result.stderr and 'no crontab' not in result.stderr.lower():
            raise NetworkError('Cannot read current crontab; existing schedule preserved')
        lines = [line for line in result.stdout.splitlines() if '# node-network-ddns' not in line]
        if enabled:
            lines.append('*/5 * * * * /usr/bin/env python3 /usr/local/lib/node-network/node-network.py run >> /var/log/node-network.log 2>&1 # node-network-ddns')
            subprocess.run(['rc-update', 'add', 'crond', 'default'], check=True)
            subprocess.run(['rc-service', 'crond', 'start'], check=True)
        subprocess.run(['crontab', '-'], input='\n'.join(lines)+'\n', text=True, check=True)
    else:
        raise NetworkError('DDNS scheduling requires systemd or OpenRC with crontab')


def main():
    parser = argparse.ArgumentParser(description='Node public IP and optional Cloudflare DDNS (DNS-only records)')
    parser.add_argument('action', choices=['ip', 'configure', 'report-configure', 'run', 'disable', 'status'])
    parser.add_argument('--panel-url', default='')
    parser.add_argument('--domain', default='')
    parser.add_argument('--zone-id', default='')
    parser.add_argument('--token-file', type=Path)
    parser.add_argument('--family', choices=['ipv4', 'ipv6', 'dual'], default='ipv4')
    args = parser.parse_args()
    if args.action == 'ip':
        detected = False
        for typ in ['A', 'AAAA']:
            try:
                print('%s: %s' % (typ, discover(Client(), typ)))
                detected = True
            except NetworkError as error:
                print(str(error))
        return 0 if detected else 1
    if sys.platform != 'linux' or os.geteuid() != 0:
        raise NetworkError('DDNS management requires root on Linux')
    if args.action in ('configure', 'report-configure'):
        if not Path('/run/systemd/system').exists() and not (shutil.which('crontab') and shutil.which('rc-service')):
            raise NetworkError('DDNS scheduling requires systemd or OpenRC with crontab')
        if args.action == 'report-configure':
            panel = args.panel_url or input('HTTPS panel origin: ').strip()
            token = args.token_file.read_text().strip() if args.token_file else getpass.getpass('Node report key: ').strip()
            config = validate_report({'panel_url': panel, 'token': token, 'family': args.family})
        else:
            domain = args.domain or input('DNS hostname: ').strip()
            zone = args.zone_id or input('Cloudflare Zone ID: ').strip()
            token = args.token_file.read_text().strip() if args.token_file else getpass.getpass('Cloudflare API token (Zone DNS Edit): ').strip()
            config = validate({'domain': domain, 'zone_id': zone, 'token': token, 'family': args.family})
        import fcntl
        STATE.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        with open(STATE.parent / 'lock', 'w') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            print('Configuring node address reporting' if config.get('mode') == 'panel' else 'Configuring direct Cloudflare DDNS (DNS-only)')
            # Verify credentials and DNS updates before replacing a working local setup.
            execute(Client(), config)
            INSTALLED.parent.mkdir(parents=True, exist_ok=True)
            if Path(__file__).resolve() != INSTALLED.resolve():
                shutil.copyfile(__file__, INSTALLED)
            INSTALLED.chmod(0o755)
            Path('/usr/local/bin/node-network').write_text('#!/bin/sh\nexec /usr/bin/env python3 /usr/local/lib/node-network/node-network.py "$@"\n')
            Path('/usr/local/bin/node-network').chmod(0o755)
            write_private(CONFIG, config)
            write_private(STATE, {})
            schedule(True)
            print('Node network updates enabled; checks run every five minutes.')
    elif args.action == 'run':
        run()
    elif args.action == 'disable':
        schedule(False)
        import fcntl
        STATE.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        with open(STATE.parent / 'lock', 'w') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            write_private(STATE, {'paused': True})
        print('DDNS disabled. Existing DNS records have been retained.')
    else:
        if not CONFIG.exists():
            print('DDNS is not configured')
        else:
            config = json.loads(CONFIG.read_text())
            print('Domain: %s; family: %s' % (config.get('domain', config.get('panel_url', '')), config['family']))
            print('State: ' + (STATE.read_text() if STATE.exists() else '{}'))
    return 0

if __name__ == '__main__':
    try:
        sys.exit(main())
    except (NetworkError, OSError, ValueError, EOFError, subprocess.SubprocessError) as error:
        # Never emit request headers, credentials or API response bodies.
        print(str(error) if isinstance(error, NetworkError) else 'Local operation failed; check files and service manager', file=sys.stderr)
        sys.exit(1)
