"""The guest jobs' dispatch and result contract, run on any Python (the VM runs Hermes's)."""
import contextlib
import importlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import types
import unittest
from unittest import mock

# The VM's YAML reader (hermes_yaml) has PyYAML's safe_load and safe_dump; PyYAML stands in here.
sys.modules.setdefault('hermes_yaml', importlib.import_module('yaml'))

GUEST = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(GUEST))
storage = importlib.import_module('storage')
disk = importlib.import_module('disk')
update = importlib.import_module('update')
install = importlib.import_module('install')
fresh = importlib.import_module('fresh')
kit = importlib.import_module('kit')
slack = importlib.import_module('slack')
archive = importlib.import_module('archive')
backup = importlib.import_module('backup')
align = importlib.import_module('align')
restore = importlib.import_module('restore')
harvest = importlib.import_module('harvest')
selftest = importlib.import_module('selftest')


class FakeGateway:
    """Stand-ins for Hermes's gateway modules, installed into sys.modules for one test."""

    def __init__(self, pid=101, runtime=None, marker=None, platforms=('slack',)):
        self.pid, self.runtime, self.marker, self.platforms = pid, runtime, marker, platforms
        self.calls = []

    def modules(self):
        status = types.ModuleType('gateway.status')
        status.get_running_pid = lambda: self.pid
        status.read_runtime_status = lambda: self.runtime
        status.write_planned_stop_marker = lambda pid: self.calls.append(('planned_stop', pid))
        drain = types.ModuleType('gateway.drain_control')
        drain.read_drain_request = lambda: self.marker
        drain.write_drain_request = lambda principal: self.calls.append(('write', principal))
        drain.clear_drain_request = lambda: self.calls.append(('clear',))
        config = types.ModuleType('gateway.config')
        platforms = [types.SimpleNamespace(value=p) for p in self.platforms]
        config.load_gateway_config = lambda: types.SimpleNamespace(get_connected_platforms=lambda: platforms)
        return {'gateway': types.ModuleType('gateway'), 'gateway.status': status,
                'gateway.drain_control': drain, 'gateway.config': config}

    def run(self, module, argv):
        out = io.StringIO()
        with mock.patch.dict(sys.modules, self.modules()), contextlib.redirect_stdout(out):
            code = importlib.import_module(module).main(argv)
        return code, json.loads(out.getvalue())


class HermesYaml(unittest.TestCase):
    """Hermes's own environment (from its package manager) has no PyYAML but ships hermes_yaml with
    the same safe_load and safe_dump: jobs that read or write YAML use it there."""

    def test_jobs_use_hermes_yaml_where_pyyaml_is_absent(self):
        import yaml as real
        shim = types.ModuleType('hermes_yaml')
        shim.used = []
        shim.safe_load = lambda text: (shim.used.append('load'), real.safe_load(text))[1]
        shim.safe_dump = lambda data, **kw: (shim.used.append('dump'), real.safe_dump(data, **kw))[1]
        import tempfile
        with tempfile.TemporaryDirectory() as tmp, mock.patch.dict(sys.modules, {'yaml': None, 'hermes_yaml': shim}):
            config = Path(tmp) / 'config.yaml'
            config.write_text('terminal:\n  cwd: /old\nplatforms:\n  slack:\n    enabled: true\n')
            importlib.import_module('restore').set_workspace(config, '/home/agent/workspace')
            self.assertEqual(real.safe_load(config.read_text())['terminal']['cwd'], '/home/agent/workspace')
            importlib.import_module('slack').only_slack(Path(tmp))
            self.assertEqual(shim.used, ['load', 'dump', 'load'])


class Dispatch(unittest.TestCase):
    def run_guest(self, *args):
        return subprocess.run([sys.executable, '-B', str(GUEST), *args], capture_output=True, text=True, timeout=30)

    def test_unknown_job_is_a_usage_error(self):
        result = self.run_guest('no-such-job')
        self.assertEqual(result.returncode, 2)
        self.assertIn('selftest', result.stderr)

    def test_job_usage_error(self):
        self.assertEqual(self.run_guest('storage', '1').returncode, 2)

    def test_a_dev_job_runs_only_where_its_module_is_shipped(self):
        # A name without a module is unknown.
        self.assertEqual(self.run_guest('nosuchmodule').returncode, 2)
        self.assertEqual(self.run_guest('stopped').returncode, 2)  # a release job's module is not a job name
        for helper in ('archive', 'install'):  # helper modules are never jobs
            self.assertEqual(self.run_guest(helper).returncode, 2)


class Disk(unittest.TestCase):
    def test_answers_what_the_root_filesystem_holds(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = disk.main([])
        answer = json.loads(out.getvalue())
        stat = os.statvfs("/")
        self.assertEqual((code, answer["status"]), (0, "passed"))
        self.assertGreater(answer["used_bytes"], 0)
        self.assertLessEqual(answer["used_bytes"], stat.f_blocks * stat.f_frsize)

    def test_takes_no_arguments(self):
        self.assertEqual(disk.main(['x']), 2)


class Storage(unittest.TestCase):
    def main(self, check):
        original = storage.check
        storage.check = check
        out = io.StringIO()
        try:
            with contextlib.redirect_stdout(out):
                code = storage.main(['200', '0', '-'])
        finally:
            storage.check = original
        return code, json.loads(out.getvalue())

    def test_failure_prints_only_the_class(self):
        def check(*_):
            raise AssertionError('step=storage class=device_capacity_mismatch')
        code, result = self.main(check)
        self.assertEqual(code, 1)
        self.assertEqual(result, {'step': 'storage', 'status': 'failed', 'class': 'device_capacity_mismatch'})

    def test_pass_prints_the_result_and_maps_no_workspace(self):
        seen = []
        def check(root, docker, workspace):
            seen.append((root, docker, workspace))
            return {'step': 'storage', 'status': 'passed'}
        code, result = self.main(check)
        self.assertEqual((code, result['status'], seen), (0, 'passed', [(200, 0, '')]))


class Drain(unittest.TestCase):
    def test_engage_writes_the_marker_and_reports_fresh_draining(self):
        fake = FakeGateway(runtime={'gateway_state': 'draining', 'active_agents': 1, 'updated_at': '2026-01-02T03:04:06Z',
                                    'active_work': [{'kind': 'cron', 'elapsed_s': 180}, {'kind': 'chat'}, 'junk']},
                           marker={'requested_at': '2026-01-02T03:04:05Z'})
        code, result = fake.run('drain', ['engage'])
        self.assertEqual(code, 0)
        self.assertEqual(fake.calls, [('write', 'operator-drain')])
        self.assertEqual(result, {'running': True, 'marker': True, 'state': 'draining', 'active': 1,
                                  'fresh': True, 'work': ['cron 3m', 'chat']})

    def test_status_older_than_the_marker_is_not_fresh(self):
        fake = FakeGateway(runtime={'gateway_state': 'draining', 'updated_at': '2026-01-02T03:04:04Z'},
                           marker={'requested_at': '2026-01-02T03:04:05Z'})
        self.assertFalse(fake.run('drain', ['status'])[1]['fresh'])

    def test_cancel_clears_and_status_only_reads(self):
        fake = FakeGateway(pid=None)
        self.assertEqual(fake.run('drain', ['cancel'])[1]['running'], False)
        fake.run('drain', ['status'])
        self.assertEqual(fake.calls, [('clear',)])


class StopGateway(unittest.TestCase):
    def test_already_stopped_clears_the_drain(self):
        fake = FakeGateway(pid=None)
        self.assertEqual(fake.run('stop', []), (0, {'status': 'passed', 'state': 'already_stopped'}))
        self.assertEqual(fake.calls, [('clear',)])

    def test_stops_with_the_planned_stop_marker(self):
        fake = FakeGateway(pid=999999)
        with mock.patch('os.kill') as kill:
            def gone(pid, sig):
                fake.pid = None
            kill.side_effect = gone
            self.assertEqual(fake.run('stop', [])[1], {'status': 'passed', 'state': 'stopped'})
        self.assertEqual(fake.calls, [('planned_stop', 999999), ('clear',)])

    def test_a_gateway_that_never_exits_fails(self):
        fake = FakeGateway(pid=999999)
        stop = importlib.import_module('stop')
        with mock.patch('os.kill'), mock.patch.object(stop, 'WAIT_SECONDS', 0.01):
            code, result = fake.run('stop', [])
        self.assertEqual((code, result), (1, {'status': 'failed', 'class': 'gateway_still_running'}))


class Ready(unittest.TestCase):
    @staticmethod
    def live(state='connected', **extra):
        import os
        return {'gateway_state': 'running', 'pid': os.getpid(), 'platforms': {'slack': {'state': state, **extra}}}

    def test_ready_with_slack(self):
        import os
        fake = FakeGateway(pid=os.getpid(), runtime=self.live())
        self.assertEqual(fake.run('ready', ['--platform', 'slack']), (0, {'status': 'passed'}))

    def test_failure_classes(self):
        import os
        pid = os.getpid()
        cases = [
            (FakeGateway(pid=None), 'gateway_not_running'),
            (FakeGateway(pid=pid, runtime={'gateway_state': 'starting'}), 'gateway_not_ready'),
            (FakeGateway(pid=pid, runtime=self.live(), platforms=()), 'platform_not_connected'),
            (FakeGateway(pid=pid, runtime=self.live(), platforms=('slack', 'web')), 'platform_not_connected'),
            # Configured but not live: still connecting, needing attention, or another process's status.
            (FakeGateway(pid=pid, runtime=self.live('connecting')), 'platform_not_connected'),
            (FakeGateway(pid=pid, runtime=self.live(needs_attention=True)), 'platform_not_connected'),
            (FakeGateway(pid=pid, runtime={**self.live(), 'pid': pid + 1}), 'platform_not_connected'),
        ]
        for fake, failure in cases:
            self.assertEqual(fake.run('ready', ['--platform', 'slack']), (1, {'status': 'failed', 'class': failure}))

    def test_without_platform_any_connection_will_do(self):
        import os
        fake = FakeGateway(pid=os.getpid(), runtime={'gateway_state': 'running'}, platforms=())
        self.assertEqual(fake.run('ready', [])[0], 0)


class Fresh(unittest.TestCase):
    def setUp(self):
        import tempfile
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(__import__('shutil').rmtree, self.tmp)
        self.home, self.workspace = self.tmp / 'home', self.tmp / 'workspace'
        self.home.mkdir()
        self.meminfo = self.tmp / 'meminfo'
        self.meminfo.write_text('MemTotal:       12000000 kB\n')
        import hashlib, os
        self.request = {'cpus': os.cpu_count(), 'memory_gib': 12, 'root_gib': 1, 'docker_gib': 1,
                        'config_sha256': hashlib.sha256(b'seed').hexdigest()}

    def check(self, pid=None, request=None):
        status = types.ModuleType('gateway.status')
        status.get_running_pid = lambda: pid
        with mock.patch.dict(sys.modules, {'gateway': types.ModuleType('gateway'), 'gateway.status': status}):
            fresh.check(request or self.request, home=self.home, workspace=self.workspace,
                        meminfo=self.meminfo, docker=lambda args: b'')

    def test_a_fresh_vm_passes(self):
        self.check()

    def test_only_the_seeded_config_backup_is_allowed(self):
        for name, content, allowed in [
                ('backups/config/config.yaml.good.20260916-195009', b'seed', True),
                ('backups/config/config.yaml.good.20260916-195009', b'old config', False),
                ('backups/state.zip', b'seed', False),
                ('memories/MEMORY.md', b'old solution', False),
                ('sessions/prior.json', b'old conversation', False)]:
            with self.subTest(name=name, content=content):
                import shutil
                shutil.rmtree(self.home)
                path = self.home / name
                path.parent.mkdir(parents=True)
                path.write_bytes(content)
                if allowed:
                    self.check()
                else:
                    with self.assertRaises(fresh.Failure):
                        self.check()

    def test_old_work_or_a_wrong_size_fails(self):
        self.workspace.mkdir()
        (self.workspace / 'left').write_text('x')
        with self.assertRaisesRegex(fresh.Failure, 'workspace_not_empty'):
            self.check()
        (self.workspace / 'left').unlink()
        with self.assertRaisesRegex(fresh.Failure, 'gateway_started_before_initialization'):
            self.check(pid=7)
        with self.assertRaisesRegex(fresh.Failure, 'cpu_count_mismatch'):
            self.check(request={**self.request, 'cpus': 999})
        with self.assertRaisesRegex(fresh.Failure, 'memory_mismatch'):
            self.check(request={**self.request, 'memory_gib': 64})


class Kit(unittest.TestCase):
    def run_kit(self, source, argv=('seed-check.py', 'check')):
        import tempfile, shutil
        folder = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, folder)
        (folder / 'kit').mkdir()
        if source is not None:
            (folder / 'kit' / argv[0]).write_text(source)
        out = io.StringIO()
        with mock.patch.object(kit, 'HERE', folder), mock.patch('subprocess.run', wraps=subprocess.run) as run, \
                contextlib.redirect_stdout(out):
            code = kit.main(list(argv))
        return code, (json.loads(out.getvalue()) if out.getvalue() else None), run

    def test_the_tool_runs_with_the_seeds_and_request_named(self):
        code, result, run = self.run_kit('import os, sys\nprint(os.environ["TROUPE_SEEDS"].endswith("seeds"), sys.argv[1])\n')
        self.assertEqual((code, result['status']), (0, 'passed'))
        self.assertIn('True check', result['output'])
        self.assertEqual(run.call_args.kwargs['stdin'], subprocess.DEVNULL)

    def test_failure_class_comes_from_the_tool(self):
        code, result, _ = self.run_kit('import sys\nprint("step=x status=failed class=assertion detail=bad", file=sys.stderr)\nsys.exit(1)\n')
        self.assertEqual((code, result['status'], result['class']), (1, 'failed', 'assertion'))
        code, result, _ = self.run_kit('raise SystemExit(3)\n')
        self.assertEqual(result['class'], 'kit_tool_failed')

    def test_missing_or_oddly_named_tools_are_refused(self):
        self.assertEqual(self.run_kit(None)[1]['class'], 'kit_tool_missing')
        for argv in (['../x.py', 'check'], ['x.py', 'Check'], ['x.sh', 'check']):
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(kit.main(argv), 2)


class SlackApiError(Exception):
    def __init__(self, code):
        super().__init__('private response content')
        self.response = {'error': code}


class Slack(unittest.TestCase):
    request = {'home': {'scope_id': 'T1'}, 'scopes': ['chat:write', 'im:history'],
               'boundary': {'public': 'C0PUB', 'private': 'C0PRIV'},
               'tokens': {'SLACK_BOT_TOKEN': 'sbx-cs-ada-slack-bot', 'SLACK_APP_TOKEN': 'sbx-cs-ada-slack-app'}}

    def client(self, scopes='chat:write,im:history', refuse='not_in_channel', missing='missing_scope'):
        client = mock.Mock()
        auth = {'ok': True, 'bot_id': 'B1', 'user_id': 'U0BOT', 'team_id': 'T1'}
        client.auth_test.return_value = mock.Mock(headers={'x-oauth-scopes': scopes},
                                                  get=auth.get, __getitem__=lambda s, k: auth[k])
        def refused(*a, **k):
            raise SlackApiError(refuse)
        for name in ('conversations_replies', 'reactions_get', 'reactions_add', 'chat_postMessage', 'files_completeUploadExternal'):
            getattr(client, name).side_effect = refused
        def history(channel, **k):
            if channel != 'C0HOME':
                raise SlackApiError(refuse)
            return {'messages': []}
        client.conversations_history.side_effect = history
        for name in ('users_list', 'conversations_list', 'conversations_open'):
            getattr(client, name).side_effect = lambda *a, **k: (_ for _ in ()).throw(SlackApiError(missing))
        client.files_getUploadURLExternal.return_value = {'file_id': 'F1', 'upload_url': 'https://upload.invalid'}
        client.files_list.return_value = {'files': [], 'paging': {'pages': 1}}
        client.apps_connections_open.return_value = {'ok': True, 'url': 'wss://x'}
        return client

    def home(self, platforms=('slack',)):
        import tempfile, shutil, yaml
        home = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, home)
        (home / '.env').write_text('SLACK_MANAGER=U0ONE\nSLACK_HOME_CHANNEL=U0ONE\nSLACK_ALLOWED_USERS=U0ONE\n'
                                   'SLACK_BOT_TOKEN=old\nOTHER=kept\n')
        (home / 'config.yaml').write_text(yaml.safe_dump({'platforms': {p: {'enabled': True} for p in platforms}}))
        return home

    def setup(self, client, home, request=None, probes=True):
        status = types.ModuleType('gateway.status')
        status.get_running_pid = lambda: None
        uploads = types.SimpleNamespace(post=mock.Mock(return_value=mock.Mock(raise_for_status=mock.Mock())))
        with mock.patch.dict(sys.modules, {'gateway': types.ModuleType('gateway'), 'gateway.status': status, 'requests': uploads}):
            return slack.setup(request or self.request, client, home, probes=probes)

    def test_a_move_checks_the_vm_not_the_app(self):
        # A crossed boundary would fail a setup; a move never probes it, nor the scopes.
        client = self.client(scopes='chat:write,im:history,users:read')
        client.chat_postMessage.side_effect = None
        home = self.home()
        self.assertEqual(self.setup(client, home, probes=False), 'T1')
        for name in ('users_list', 'conversations_list', 'conversations_open', 'chat_postMessage', 'files_list'):
            getattr(client, name).assert_not_called()
        # It still rewrites .env, authenticates both tokens and holds the workspace.
        self.assertIn('SLACK_BOT_TOKEN=sbx-cs-ada-slack-bot', (home / '.env').read_text())
        client.auth_test.assert_called_once()
        client.apps_connections_open.assert_called_once()
        with self.assertRaisesRegex(slack.Failure, 'slack_workspace_or_bot_mismatch'):
            self.setup(self.client(), self.home(), self.published(scope_id='T9'), probes=False)

    def test_each_boundary_channel_is_probed(self):
        # Only the private channel lets the bot read: it must be the one named, so every probe
        # reached its own channel (a closure reading the loop variable late would miss it).
        client = self.client()
        def history(channel, **k):
            if channel == 'C0PRIV':
                return {'messages': []}
            raise SlackApiError('not_in_channel')
        client.conversations_history.side_effect = history
        with self.assertRaisesRegex(slack.Failure, 'slack_boundary_crossed_history_private'):
            self.setup(client, self.home())
        # A readable boundary channel stops the run before any probe writes anywhere.
        client.chat_postMessage.assert_not_called()
        client.reactions_add.assert_not_called()
        client.files_getUploadURLExternal.assert_not_called()

    def test_the_boundary_probes_all_run(self):
        # Run at once, every probe of both channels is still made: 6 calls per channel.
        client = self.client()
        self.setup(client, self.home())
        self.assertEqual(client.chat_postMessage.call_count, 2)
        self.assertEqual(client.reactions_add.call_count, 2)
        self.assertEqual(client.files_getUploadURLExternal.call_count, 2)

    def published(self, **home):
        """A published agent's request: its workspace not recorded yet, and the given boundary."""
        import copy
        request = copy.deepcopy(self.request)
        request['home'].update(home)
        return request

    def test_a_first_setup_learns_the_workspace_from_the_token(self):
        request = self.published(scope_id='')
        self.assertEqual(self.setup(self.client(), self.home(), request), 'T1')
        self.assertEqual(request['home']['scope_id'], 'T1')

    def test_a_recorded_workspace_holds(self):
        with self.assertRaisesRegex(slack.Failure, 'slack_workspace_or_bot_mismatch'):
            self.setup(self.client(), self.home(), self.published(scope_id='T9'))

    def test_without_boundary_channels_none_is_checked(self):
        request = self.published(scope_id='')
        request['boundary'] = {}
        client = self.client()
        self.assertEqual(self.setup(client, self.home(), request), 'T1')
        client.chat_postMessage.assert_not_called()

    def test_an_empty_boundary_channel_is_refused_not_passed(self):
        request = self.published()
        request['boundary'] = {'public': '', 'private': 'C0PRIV'}
        with self.assertRaisesRegex(slack.Failure, 'slack_boundary_channel_malformed'):
            self.setup(self.client(), self.home(), request)

    def test_a_probe_needs_a_known_workspace(self):
        with self.assertRaisesRegex(slack.Failure, 'slack_workspace_unknown'):
            slack.probe(self.client(), self.published(scope_id=''))

    def test_setup_writes_the_placeholders_and_keeps_the_rest(self):
        home = self.home()
        self.setup(self.client(), home)
        env = (home / '.env').read_text()
        self.assertEqual(env, 'SLACK_MANAGER=U0ONE\nSLACK_HOME_CHANNEL=U0ONE\nSLACK_ALLOWED_USERS=U0ONE\nOTHER=kept\n'
                              'SLACK_ALLOW_BOTS=none\n'
                              'SLACK_APP_TOKEN=sbx-cs-ada-slack-app\nSLACK_BOT_TOKEN=sbx-cs-ada-slack-bot\n')

    def test_each_boundary_failure_stops_setup(self):
        for client, platforms, reason in (
                (self.client(), ('slack', 'web'), 'slack_not_the_only_channel'),
                (self.client(scopes='chat:write,im:history,users:read'), ('slack',), 'slack_scopes_differ_from_manifest'),
                (self.client(missing='ratelimited'), ('slack',), 'slack_users_list_not_refused'),
                (self.client(refuse='ratelimited'), ('slack',), 'slack_boundary_check_incomplete')):
            with self.subTest(reason=reason), self.assertRaisesRegex(slack.Failure, reason):
                self.setup(client, self.home(platforms))

    def test_the_live_settings_are_the_managers_but_keep_their_shape(self):
        boundary = self.request['boundary']
        base = {'SLACK_MANAGER': 'U0ONE', 'SLACK_ALLOWED_USERS': 'U0ONE,W0TWO', 'SLACK_HOME_CHANNEL': 'U0ONE'}
        for change, reason in (({}, None),
                               ({'SLACK_HOME_CHANNEL': 'C0PROJECT'}, None),
                               ({'SLACK_HOME_CHANNEL': 'D0DM'}, None),
                               ({'SLACK_MANAGER': 'W0TWO', 'SLACK_HOME_CHANNEL': 'W0TWO'}, None),
                               ({'SLACK_MANAGER': ''}, 'slack_manager_missing'),
                               ({'SLACK_MANAGER': 'U0THREE'}, 'slack_manager_not_allowlisted'),
                               ({'SLACK_ALLOWED_USERS': 'U0ONE,*'}, 'slack_allowlist_widened'),
                               ({'GATEWAY_ALLOW_ALL_USERS': 'true'}, 'slack_allowlist_widened'),
                               ({'SLACK_HOME_CHANNEL': 'W0TWO'}, 'slack_home_invalid'),
                               ({'SLACK_HOME_CHANNEL': 'C0PRIV'}, 'slack_home_invalid'),
                               ({'SLACK_HOME_CHANNEL': ''}, 'slack_home_invalid')):
            home = self.home()
            (home / '.env').write_text(''.join(f'{k}={v}\n' for k, v in {**base, **change}.items()))
            with self.subTest(change=change):
                if reason is None:
                    slack.check_live_slack(boundary, home)
                else:
                    with self.assertRaisesRegex(slack.Failure, reason):
                        slack.check_live_slack(boundary, home)

    def test_a_crossed_boundary_is_named(self):
        client = self.client()
        client.chat_postMessage.side_effect = None
        with self.assertRaisesRegex(slack.Failure, 'slack_boundary_crossed_post_private'):
            self.setup(client, self.home())

    def test_failures_never_carry_slack_answers(self):
        self.assertEqual(slack.failure_class(SlackApiError('invalid_auth')), 'slack_invalid_auth')
        self.assertEqual(slack.failure_class(SlackApiError('something_new')), 'slack_api_error')
        self.assertEqual(slack.failure_class(KeyError('xoxb-secret')), 'slack_keyerror')


def gateway(pid=None):
    status = types.ModuleType('gateway.status')
    status.get_running_pid = lambda: pid
    return mock.patch.dict(sys.modules, {'gateway': types.ModuleType('gateway'), 'gateway.status': status})


def make_zip(path, entries, db_ok=True):
    import sqlite3, tempfile, zipfile
    with zipfile.ZipFile(path, 'w') as z:
        for name, data in entries.items():
            info = zipfile.ZipInfo(name)
            info.external_attr = (data[1] if isinstance(data, tuple) else 0o100600) << 16
            z.writestr(info, data[0] if isinstance(data, tuple) else data)
        if db_ok is not None:
            with tempfile.TemporaryDirectory() as tmp:
                db = Path(tmp) / 'state.db'
                conn = sqlite3.connect(db)
                conn.execute('CREATE TABLE sessions (id)')
                conn.commit()
                conn.close()
                raw = db.read_bytes()
                z.writestr('state.db', raw if db_ok else raw[:100] + b'\0' * (len(raw) - 100))


class Archive(unittest.TestCase):
    def test_databases_and_crcs_are_checked(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            good, bad = Path(tmp) / 'good.zip', Path(tmp) / 'bad.zip'
            make_zip(good, {'config.yaml': b'x'})
            make_zip(bad, {'config.yaml': b'x'}, db_ok=False)
            self.assertEqual(archive.problem(good), '')
            self.assertIn(archive.problem(bad), ('sqlite_integrity_failed', 'archive_invalid'))
            (Path(tmp) / 'junk.zip').write_bytes(b'not a zip')
            self.assertEqual(archive.problem(Path(tmp) / 'junk.zip'), 'archive_invalid')


class ArchiveNames(unittest.TestCase):
    def test_only_names_the_restore_writes_exactly(self):
        for name in ('state.db', 'memories/MEMORY.md', 'skills/', '_external/workspace/notes.md', '_external/', '_external/workspace/'):
            self.assertEqual(archive.name_problem(name), '', name)
        for name in ('/abs', '../up', 'a/../b', '.', './x', 'a//b', 'a/./b', '', 'a\\b', 'a\x00b'):
            self.assertEqual(archive.name_problem(name), 'unsafe_archive_path', name)
        for name in ('_external/.bashrc', '_external/.local/bin/hermes', '_external/workspacex/y'):
            self.assertEqual(archive.name_problem(name), 'archive_external_path_not_allowed', name)

    def test_a_backup_with_a_bad_name_is_refused_in_the_vm(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'state.zip'
            make_zip(path, {'_external/.bashrc': b'x'})
            self.assertEqual(archive.problem(path), 'archive_external_path_not_allowed')

    def test_the_unicode_path_field_is_judged_as_the_restore_reads_it(self):
        # Python's zipfile renames an entry from its Info-ZIP Unicode Path field (0x7075); the
        # check and the restore both use that same parser, so they agree on the name.
        import tempfile, zipfile, zlib, struct
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'state.zip'
            raw, real = b'_external/workspace/x', '_external/.bashrc'
            field = struct.pack('<B', 1) + struct.pack('<I', zlib.crc32(raw)) + real.encode()
            with zipfile.ZipFile(path, 'w') as z:
                info = zipfile.ZipInfo(raw.decode())
                info.extra = struct.pack('<HH', 0x7075, len(field)) + field
                z.writestr(info, b'x')
            with zipfile.ZipFile(path) as z:
                seen = z.namelist()[0]
            self.assertEqual(archive.problem(path), archive.name_problem(seen) or archive.problem(path))
            if seen == real:
                self.assertEqual(archive.problem(path), 'archive_external_path_not_allowed')


class Backup(unittest.TestCase):
    def test_make_records_the_lock_and_commit_and_clean_restores_it(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            home, path = Path(tmp) / 'home', Path(tmp) / 'out.zip'
            home.mkdir()
            def run(args, **kwargs):
                if args[1] == 'backup':
                    make_zip(Path(args[3]), {'config.yaml': b'x'})
                    (home / '.backup.lock').touch()
                    return types.SimpleNamespace(returncode=0, stdout='', stderr='')
                return types.SimpleNamespace(returncode=0, stdout='a' * 40 + '\n')
            with mock.patch.object(backup.subprocess, 'run', side_effect=run):
                result = backup.make(home=home, path=path)
            import hashlib
            self.assertEqual(result, {'status': 'passed', 'lock_existed': False, 'commit': 'a' * 40,
                                      'sha256': hashlib.sha256(path.read_bytes()).hexdigest(), 'size': path.stat().st_size})
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            backup.clean(False, home=home, path=path)
            self.assertFalse(path.exists() or (home / '.backup.lock').exists())

    def test_a_failed_native_backup_is_reported(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            with mock.patch.object(backup.subprocess, 'run', return_value=types.SimpleNamespace(returncode=1, stderr='')):
                self.assertEqual(backup.make(home=Path(tmp), path=Path(tmp) / 'x.zip')['class'], 'native_backup_failed')


class Align(unittest.TestCase):
    def run_align(self, update=0, contained=(0,), pid=None):
        """contained: the merge-base answers in order (before the update, then after it)."""
        answers, calls = list(contained), []

        def run(args, **kwargs):
            calls.append('update' if 'update' in args else 'merge-base' if 'merge-base' in args else 'other')
            code = update if 'update' in args else answers.pop(0) if 'merge-base' in args else 0
            return types.SimpleNamespace(returncode=code, stdout='b' * 40 + '\n')
        with gateway(pid):
            return align.align('a' * 40, run=run), calls

    def test_an_image_that_holds_the_commit_is_not_updated(self):
        result, calls = self.run_align(contained=(0,))
        self.assertEqual(result, {'status': 'passed', 'commit': 'b' * 40, 'updated': False})
        self.assertNotIn('update', calls)

    def test_the_preserved_commit_must_be_contained(self):
        result, calls = self.run_align(contained=(1, 0))
        self.assertEqual(result, {'status': 'passed', 'commit': 'b' * 40, 'updated': True})
        self.assertEqual(calls[:3], ['merge-base', 'update', 'merge-base'])
        self.assertEqual(self.run_align(contained=(1, 1))[0]['class'], 'preserved_commit_not_contained')
        self.assertEqual(self.run_align(update=1, contained=(1,))[0]['class'], 'hermes_update_failed')
        self.assertEqual(self.run_align(pid=7)[0]['class'], 'gateway_running')

    def test_only_a_commit_is_accepted(self):
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(align.main(['main']), 2)


class Restore(unittest.TestCase):
    def setUp(self):
        import tempfile, shutil
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        self.home, self.user = self.tmp / 'agent/.hermes', self.tmp / 'agent'
        self.workspace = self.tmp / 'agent/workspace'
        self.home.mkdir(parents=True)
        self.workspace.mkdir()
        (self.home / 'seeded').write_text('seed')
        self.archive = self.tmp / 'state.zip'

    def restore(self, entries, pid=None):
        make_zip(self.archive, {'config.yaml': b'terminal: {cwd: /old}\n', **entries})
        hermes_cli, layout = types.ModuleType('hermes_cli'), types.ModuleType('hermes_cli.home_data_layout')
        layout.PM_RUNTIME_ROOT_DIRS = frozenset({'installs', 'tools', 'cache'})
        with gateway(pid), mock.patch.dict(sys.modules, {'hermes_cli': hermes_cli, 'hermes_cli.home_data_layout': layout}):
            restore.restore(path=self.archive, home=self.home, user_home=self.user, workspace=str(self.workspace),
                            migrate=lambda: None, commit='c' * 40)

    def test_state_replaces_the_seeded_home(self):
        import yaml
        self.restore({'memories/MEMORY.md': b'remembered', 'gateway.pid': b'99', '_external/workspace/notes.md': b'note',
                      'scripts/run.sh': (b'#!/bin/sh', 0o100755)})
        self.assertFalse((self.home / 'seeded').exists())
        self.assertEqual((self.home / 'memories/MEMORY.md').read_bytes(), b'remembered')
        self.assertFalse((self.home / 'gateway.pid').exists())
        self.assertEqual((self.workspace / 'notes.md').read_bytes(), b'note')
        self.assertEqual((self.home / 'scripts/run.sh').stat().st_mode & 0o777, 0o700)
        self.assertEqual(yaml.safe_load((self.home / 'config.yaml').read_text())['terminal']['cwd'], str(self.workspace))
        self.assertIn('factory reset', (self.home / 'workstation/journal.md').read_text())
        self.assertFalse(self.archive.exists())

    def test_hermess_own_environment_survives_and_the_archive_cannot_replace_it(self):
        # align has just built Hermes's environment here; the restore and every later job run on it.
        python = self.home / 'installs/k/environments/g/venv/bin/python'
        for path in (python, self.home / 'tools/python-3.14/bin/python3', self.home / 'cache/uv/entry'):
            path.parent.mkdir(parents=True)
            path.write_text('built')
        self.restore({'installs/k/environments/g/venv/bin/python': b'planted', 'tools/python-3.14/bin/python3': b'planted',
                      'cache/images/sent.png': b'media', 'memories/MEMORY.md': b'remembered'})
        for path in (python, self.home / 'tools/python-3.14/bin/python3', self.home / 'cache/uv/entry'):
            self.assertEqual(path.read_text(), 'built')
        # The backup keeps only cache/'s durable artifacts (delivered media, citations); they come back.
        self.assertEqual((self.home / 'cache/images/sent.png').read_bytes(), b'media')
        self.assertFalse((self.home / 'seeded').exists())
        self.assertEqual((self.home / 'memories/MEMORY.md').read_bytes(), b'remembered')

    def test_unsafe_archives_and_a_running_gateway_change_nothing(self):
        import stat
        for entries, pid, reason in (({'../escape': b'x'}, None, 'unsafe_archive_path'),
                                     # Persistence outside the workspace never survives a reset.
                                     ({'_external/.bashrc': b'x'}, None, 'archive_external_path_not_allowed'),
                                     ({'_external/.local/bin/hermes': b'x'}, None, 'archive_external_path_not_allowed'),
                                     ({'link': (b'/etc/passwd', stat.S_IFLNK | 0o777)}, None, 'archive_symlink'),
                                     ({}, 7, 'gateway_running')):
            with self.subTest(reason=reason):
                with self.assertRaisesRegex(restore.Failure, reason):
                    self.restore(entries, pid)
                self.assertTrue((self.home / 'seeded').exists())


class Continuity(unittest.TestCase):
    def client(self, history, replies=()):
        client = mock.Mock()
        client.auth_test.return_value = {'team_id': 'T1', 'user_id': 'U0BOT'}
        client.conversations_list.return_value = {'channels': [{'id': 'D0ONE', 'user': 'U0ONE'}, {'id': 'D0OTHER', 'user': 'U0STRANGER'}]}
        client.conversations_history.side_effect = lambda channel, **k: {'messages': history if channel == 'D0ONE' else
                                                                         [{'user': 'U0STRANGER', 'text': '!status', 'ts': '5.0'}]}
        client.conversations_replies.return_value = {'messages': list(replies)}
        return client

    def check(self, client):
        import tempfile, shutil
        home = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, home)
        (home / '.env').write_text('SLACK_MANAGER=U0ONE\nSLACK_HOME_CHANNEL=U0ONE\nSLACK_ALLOWED_USERS=U0ONE\n')
        return slack.continuity({'home': {'scope_id': 'T1'}, 'boundary': {'public': 'C0PUB'}, 'since': 1.0}, client, home)

    def test_any_allowlisted_message_and_the_bots_later_reply_count(self):
        for text in ('hello', '!status'):
            with self.subTest(text=text):
                result = self.check(self.client([{'user': 'U0ONE', 'text': text, 'ts': '3.0'},
                                                  {'user': 'U0BOT', 'text': 'All good', 'ts': '4.0'}]))
                self.assertEqual(result, {'observed': True, 'request_ts': '3.0', 'reply_ts': '4.0', 'channel': 'D0ONE'})

    def test_strangers_old_replies_and_events_do_not(self):
        for history in ([{'user': 'U0ONE', 'subtype': 'channel_join', 'text': 'joined', 'ts': '3.0'},
                         {'user': 'U0BOT', 'text': 'hi', 'ts': '4.0'}],
                        [{'user': 'U0BOT', 'text': 'earlier', 'ts': '2.0'}, {'user': 'U0ONE', 'text': 'hello', 'ts': '3.0'}],
                        [{'user': 'U0ONE', 'text': 'hello', 'ts': '3.0'}],
                        [{'user': 'U0ONE', 'bot_id': 'B9', 'text': 'a bot', 'ts': '3.0'}, {'user': 'U0BOT', 'text': 'hi', 'ts': '4.0'}]):
            with self.subTest(history=history):
                self.assertEqual(self.check(self.client(history)), {'observed': False})


OLD, NEW = b'---\nname: demo\n---\nold\n', b'---\nname: demo\n---\nnew\n'


def sha(data):
    import hashlib
    return hashlib.sha256(data).hexdigest()


class Update(unittest.TestCase):
    def setUp(self):
        import tempfile
        self.folder = tempfile.TemporaryDirectory()
        self.root = Path(self.folder.name)
        self.home = self.root / '.hermes'
        (self.home / 'skills/demo').mkdir(parents=True)
        (self.home / 'skills/demo/SKILL.md').write_bytes(OLD)
        (self.home / 'skills/gone').mkdir()
        (self.home / 'skills/gone/SKILL.md').write_bytes(b'gone\n')

    def tearDown(self):
        self.folder.cleanup()

    def request(self, **extra):
        import base64
        def item(before, after):
            return {'before': sha(before) if before is not None else None, 'after': sha(after) if after is not None else None,
                    'b64': base64.b64encode(after).decode() if after is not None else None,
                    'original_b64': base64.b64encode(before).decode() if before is not None else None}
        return {'files': {'skills/demo/SKILL.md': item(OLD, NEW), 'skills/new/SKILL.md': item(None, b'new\n'),
                          'skills/gone/SKILL.md': item(b'gone\n', None)}, **extra}

    def test_install_writes_private_files_and_removes_emptied_folders(self):
        self.assertEqual(install.install(self.request(), self.home), {'status': 'passed'})
        self.assertEqual((self.home / 'skills/demo/SKILL.md').read_bytes(), NEW)
        new = self.home / 'skills/new/SKILL.md'
        self.assertEqual(new.read_bytes(), b'new\n')
        self.assertEqual(oct(new.stat().st_mode & 0o777), '0o600')
        self.assertEqual(oct(new.parent.stat().st_mode & 0o777), '0o700')
        self.assertFalse((self.home / 'skills/gone').exists())
        self.assertTrue((self.home / 'skills').is_dir())
        state = update.state({'files': ['skills/demo/SKILL.md', 'skills/gone/SKILL.md']}, self.home)
        self.assertEqual(state['files']['skills/demo/SKILL.md']['sha256'], sha(NEW))
        self.assertIsNone(state['files']['skills/gone/SKILL.md'])

    def test_failure_after_a_write_restores_everything(self):
        value = install.install(self.request(fail_after_first_write=True), self.home)
        self.assertEqual((value['class'], value['restored']), ('forced_failure', True))
        self.assertEqual((self.home / 'skills/demo/SKILL.md').read_bytes(), OLD)
        self.assertFalse((self.home / 'skills/new/SKILL.md').exists())
        self.assertEqual((self.home / 'skills/gone/SKILL.md').read_bytes(), b'gone\n')

    def test_symlinked_parent_is_refused_and_everything_restored(self):
        outside = self.root / 'outside'
        outside.mkdir()
        (self.home / 'skills/new').symlink_to(outside)
        value = install.install(self.request(), self.home)
        self.assertEqual((value['class'], value['restored']), ('parent_symlink', True))
        self.assertEqual(list(outside.iterdir()), [])
        self.assertEqual((self.home / 'skills/demo/SKILL.md').read_bytes(), OLD)

    def test_changed_live_file_is_refused_before_any_write(self):
        (self.home / 'skills/demo/SKILL.md').write_bytes(b'edited\n')
        value = install.install(self.request(), self.home)
        self.assertEqual(value['class'], 'live_file_changed')
        self.assertEqual((self.home / 'skills/demo/SKILL.md').read_bytes(), b'edited\n')
        self.assertFalse((self.home / 'skills/new/SKILL.md').exists())

    def test_paths_outside_home_are_refused(self):
        for rel in ('../escape', '/etc/passwd', 'skills/../../escape'):
            request = self.request()
            request['files'] = {rel: request['files']['skills/new/SKILL.md']}
            self.assertEqual(install.install(request, self.home)['class'], 'path_not_allowed')
        self.assertFalse((self.root / 'escape').exists())

    def test_read_lists_folders_skips_bytecode_and_refuses_links(self):
        (self.home / 'skills/demo/__pycache__').mkdir()
        (self.home / 'skills/demo/__pycache__/x.pyc').write_bytes(b'x')
        (self.home / 'skills/linked').symlink_to(self.root)
        out = update.read({'folders': ['skills/demo', 'skills/linked', 'skills/absent'], 'files': ['SOUL.md']}, self.home)
        self.assertEqual(sorted(out['files']), ['skills/demo/SKILL.md', 'skills/linked'])
        self.assertEqual(out['files']['skills/linked'], {'error': 'symlink'})

    def test_forced_drain_lines_count_only_after_the_signal(self):
        import datetime
        logs = self.home / 'logs'
        logs.mkdir()
        line = 'INFO gateway.run: Skipping .clean_shutdown marker — drain timed out with interrupted agents'
        (logs / 'gateway.log').write_text(f'2026-10-06 15:00:00,000 {line}\n2026-10-06 16:30:17,230 {line}\n'
                                          '2026-10-06 16:30:18,000 INFO gateway.run: unrelated\n')
        since = datetime.datetime(2026, 10, 6, 16, 23, 44, tzinfo=datetime.timezone.utc).timestamp()
        self.assertEqual(update.state({'log_since': since}, self.home)['forced_drains'], 1)


class Harvest(unittest.TestCase):
    def test_reads_seeded_files_agent_ledger_entries_and_created_files(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            home = Path(tmp) / '.hermes'
            (home / 'skills/a').mkdir(parents=True)
            (home / 'skills/a/SKILL.md').write_text('seeded\n')
            (home / 'skills/b').mkdir()
            (home / 'skills/b/SKILL.md').write_text('created\n')
            ledger = [{'actor': 'agent', 'action': 'create', 'skill': 'b', 'ts': 't',
                       'after': [{'path': str(home / 'skills/b/SKILL.md'), 'sha256': sha(b'created\n')},
                                 {'path': '/elsewhere/x.md', 'sha256': 'y'}]},
                      {'actor': 'curator', 'action': 'patch', 'skill': 'a', 'after': []}, 'not json', '["a list"]']
            (home / 'skills/.curator_ledger.jsonl').write_text(
                '\n'.join(x if isinstance(x, str) else json.dumps(x) for x in ledger))
            result = harvest.collect({'paths': ['skills/a/SKILL.md', 'skills/missing.md']}, home)
            self.assertEqual(result['status'], 'passed')
            self.assertEqual(result['files']['skills/a/SKILL.md']['sha256'], sha(b'seeded\n'))
            self.assertIsNone(result['files']['skills/missing.md'])
            self.assertEqual([(e['ts'], e['paths']) for e in result['ledger']], [('t', ['skills/b/SKILL.md'])])
            self.assertEqual(list(result['created']), ['skills/b/SKILL.md'])

    def test_each_ledger_path_keeps_its_own_hash(self):
        # A path outside the home is dropped together with its hash, so later pairs never shift.
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            home = Path(tmp) / '.hermes'
            (home / 'skills/b').mkdir(parents=True)
            (home / 'skills/b/SKILL.md').write_text('created\n')
            entry = {'actor': 'agent', 'action': 'patch', 'skill': 'b', 'ts': 't',
                     'after': [{'path': '/elsewhere/x.md', 'sha256': 'outside'},
                               {'path': str(home / 'skills/b/SKILL.md'), 'sha256': sha(b'created\n')}]}
            (home / 'skills/.curator_ledger.jsonl').write_text(json.dumps(entry))
            result = harvest.collect({'paths': []}, home)
            self.assertEqual([(e['paths'], e['after']) for e in result['ledger']],
                             [(['skills/b/SKILL.md'], [sha(b'created\n')])])


class SelfTest(unittest.TestCase):
    """The turn passes on its usage record alone; each failure has one class."""

    def fake(self, *answers):
        """A subprocess.run that writes each answer's usage record and returns its exit and output."""
        calls = []

        def run(argv, **_):
            code, stdout, stderr, usage = answers[len(calls)]
            calls.append(argv)
            if usage is not None:
                Path(argv[argv.index('--usage-file') + 1]).write_text(json.dumps(usage))
            return types.SimpleNamespace(returncode=code, stdout=stdout, stderr=stderr)
        return run, calls

    def main(self, run, model='m-1'):
        out = io.StringIO()
        with mock.patch.object(selftest, 'configured_model', return_value=model), contextlib.redirect_stdout(out):
            code = selftest.main([], run)
        return code, json.loads(out.getvalue())

    good = {'model': 'm-1', 'api_calls': 1, 'output_tokens': 7, 'completed': True}

    def test_a_completed_turn_passes_and_returns_the_reply_capped(self):
        run, calls = self.fake((0, 'x' * 900 + '\n', '', self.good))
        code, result = self.main(run)
        self.assertEqual((code, result['status'], len(result['reply'])), (0, 'passed', 500))
        self.assertEqual(calls[0][:4], ['timeout', '300', selftest.HERMES, '-z'])

    def test_one_retry_then_the_class(self):
        cases = {
            'key_rejected': (1, '', 'Error: HTTP 401 Unauthorized', None),
            'network_unreachable': (1, '', 'ConnectError: [Errno -3] Temporary failure in name resolution', None),
            'timeout': (124, '', '', None),
            'provider_failed': (1, '', 'Error: 500 internal error', None),
            'usage_record_invalid': (0, 'hi', '', dict(self.good, model='other')),
        }
        for want, answer in cases.items():
            with self.subTest(want):
                run, calls = self.fake(answer, answer)
                code, result = self.main(run)
                self.assertEqual((code, result), (1, {'status': 'failed', 'class': want}))
                self.assertEqual(len(calls), 2)

    def test_a_retry_that_completes_passes(self):
        run, calls = self.fake((1, '', 'Error: 502', None), (0, 'ready', '', self.good))
        self.assertEqual(self.main(run), (0, {'status': 'passed', 'reply': 'ready'}))

    def test_no_configured_model_is_a_config_failure(self):
        run, calls = self.fake()
        self.assertEqual(self.main(run, model=None), (1, {'status': 'failed', 'class': 'config_unreadable'}))
        self.assertEqual(calls, [])

    def test_the_configured_model_comes_from_config_yaml(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            Path(tmp, 'config.yaml').write_text('model:\n  provider: custom:venice\n  default: z-1\n')
            self.assertEqual(selftest.configured_model(Path(tmp)), 'z-1')


if __name__ == '__main__':
    unittest.main()
