"""slack setup|move|probe|channel|continuity

The agent's Slack channel, for kits whose base kit has slack.env. Reads request.json:

    {"home": {"scope_id": TEAM}, "scopes": [...],
     "boundary": {"public": CHANNEL, "private": CHANNEL},
     "tokens": {"SLACK_BOT_TOKEN": PLACEHOLDER, "SLACK_APP_TOKEN": PLACEHOLDER}}

The agent's manager, allowlist and home are live state in its .env, which the manager changes
by telling the agent; each job checks their shape, never their values (check_live_slack).

setup, before the gateway's first start: Slack must be the only channel the config enables;
writes the token placeholders sandboxd resolves into .env, keeping its manager, allowlist and
home; proves the bot token holds exactly the app manifest's scopes, that calls it lacks are
refused and that it cannot read, react, post or upload in channels it is not in; then
authenticates both tokens. move, for a VM an agent was moved into with its Slack app and
tokens unchanged: the same .env and workspace checks and both tokens' authentication, without
the scope and boundary probes, which test the app, not the VM. probe: the bot and app tokens
authenticate in the home workspace.
channel: Slack is the only channel the config enables (before a migrate moves that config
into a new VM). continuity: with "since" in the request, whether an allowlisted member sent a
new message in an existing DM since then and the bot replied; prints
{"observed", "request_ts", "reply_ts", "channel"}. Slack's answers are never printed beyond
these fields: they can carry message content. Prints {"status", "class"}.
"""
import json
import os
from pathlib import Path
import re
import sys
import tempfile

HOME = Path('/home/agent/.hermes')
REFUSED = {'not_in_channel', 'channel_not_found'}
# A made up message: answering message_not_found or thread_not_found instead of a refusal
# would mean Slack let the bot look inside the channel.
FAKE_TS = '1000000000.000100'
# A member ID nobody holds: the conversations.open refusal probe must never involve a real person.
NOBODY = 'U00000000'
MEMBER = r'[UW][A-Z0-9]+'
ERRORS = {'channel_not_found', 'not_in_channel', 'missing_scope', 'invalid_auth', 'token_revoked',
          'account_inactive', 'user_not_found', 'ratelimited', 'not_allowed_token_type', 'is_archived'}


class Failure(Exception):
    """A controlled class, safe to report."""


def require(value, reason):
    if not value:
        raise Failure(reason)


def slack_error(error):
    return (getattr(error, 'response', None) or {}).get('error')


def failure_class(error):
    if isinstance(error, Failure):
        return str(error)
    if type(error).__name__ == 'SlackApiError':
        code = slack_error(error)
        return 'slack_' + code if code in ERRORS else 'slack_api_error'
    return 'slack_' + type(error).__name__.lower()


def env_values(text):
    values = {}
    for line in text.splitlines():
        name, sep, value = line.partition('=')
        if sep and not name.lstrip().startswith('#'):
            values[name.strip()] = value.strip()
    return values


def check_live_slack(boundary, root=HOME):
    """The live .env's manager is a member on an allowlist of explicit member IDs (Hermes reads
    '*' and GATEWAY_ALLOW* as allow everyone), and its home is a conversation (the manager's
    member ID is their DM), never a boundary channel."""
    env = env_values((root / '.env').read_text())
    manager, home = env.get('SLACK_MANAGER', ''), env.get('SLACK_HOME_CHANNEL', '')
    allowed = env.get('SLACK_ALLOWED_USERS', '').split(',')
    require(re.fullmatch(MEMBER, manager), 'slack_manager_missing')
    require(all(re.fullmatch(MEMBER, uid) for uid in allowed)
            and not any(name.startswith('GATEWAY_ALLOW') or name == 'SLACK_ALLOW_ALL_USERS' for name in env),
            'slack_allowlist_widened')
    require(manager in allowed, 'slack_manager_not_allowlisted')
    require((home == manager or re.fullmatch(r'[CGD][A-Z0-9]+', home)) and home not in boundary.values(),
            'slack_home_invalid')
    return allowed


def slack_env(text, tokens):
    """The .env with Slack's fixed settings and the token placeholders, keeping the manager,
    the allowlist, the home and every other setting."""
    keep = ('SLACK_MANAGER', 'SLACK_HOME_CHANNEL', 'SLACK_ALLOWED_USERS')
    lines = [line for line in text.splitlines()
             if not line.split('=', 1)[0].strip().startswith('SLACK_') or line.split('=', 1)[0].strip() in keep]
    lines += ['SLACK_ALLOW_BOTS=none'] + [f'{name}={value}' for name, value in sorted(tokens.items())]
    return '\n'.join(lines).strip() + '\n'


def atomic(path, data):
    require(not path.is_symlink(), 'destination_is_symlink')
    fd, name = tempfile.mkstemp(prefix='.slack-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def expect_refused(call, label):
    """The call must fail with a membership refusal; success or any other answer fails."""
    try:
        call()
    except Exception as error:
        if slack_error(error) in REFUSED:
            return
        if slack_error(error) == 'ratelimited' or slack_error(error) is None:
            raise Failure('slack_boundary_check_incomplete') from None
    raise Failure('slack_boundary_crossed_' + label)


def upload_probe(client, channel):
    """A file shared into a channel the bot is not in must be refused. The upload is private
    to the bot until shared, and is deleted either way."""
    import requests
    staged = client.files_getUploadURLExternal(filename='boundary-check.txt', length=14)
    file_id = staged['file_id']
    try:
        requests.post(staged['upload_url'], data=b'boundary check', timeout=20).raise_for_status()
        client.files_completeUploadExternal(files=[{'id': file_id, 'title': 'boundary check'}], channel_id=channel)
    finally:
        try:
            client.files_delete(file=file_id)
        except Exception:
            pass


def check_boundary(client, boundary):
    """Every probe of every boundary channel must be refused. The read-only probes run first, at
    once (each Slack call takes seconds); only when every one was refused do the writing probes
    run, so a bot that can already read a boundary channel never posts or uploads there. The
    first failure in the fixed order is reported."""
    from concurrent.futures import ThreadPoolExecutor
    reads, writes = [], []
    for kind, channel in sorted(boundary.items()):
        # An empty channel would be refused by every call, passing without testing anything.
        require(isinstance(channel, str) and re.fullmatch(r'C[A-Z0-9]+', channel), 'slack_boundary_channel_malformed')
        reads += [(f'{label}_{kind}', call) for label, call in (
                ('history', lambda c=channel: client.conversations_history(channel=c, limit=1)),
                ('replies', lambda c=channel: client.conversations_replies(channel=c, ts=FAKE_TS, limit=1)),
                ('reactions_get', lambda c=channel: client.reactions_get(channel=c, timestamp=FAKE_TS)))]
        writes += [(f'{label}_{kind}', call) for label, call in (
                ('reactions_add', lambda c=channel: client.reactions_add(channel=c, timestamp=FAKE_TS, name='eyes')),
                ('post', lambda c=channel: client.chat_postMessage(channel=c, text='Boundary check, please ignore.')),
                ('upload', lambda c=channel: upload_probe(client, c)))]

    def outcome(probe):
        try:
            expect_refused(probe[1], probe[0])
        except Failure as failure:
            return failure
        return None
    for probes in (reads, writes):
        if not probes:
            continue
        with ThreadPoolExecutor(max_workers=len(probes)) as pool:
            failures = [f for f in pool.map(outcome, probes) if f is not None]
        if failures:
            raise failures[0]


def check_scopes(client, auth, expected, boundary):
    """Exactly the manifest's scopes, the calls it lacks refused, nothing outside its channels,
    and every file it can list in one of its conversations."""
    granted = next((v for k, v in auth.headers.items() if k.lower() == 'x-oauth-scopes'), '')
    if isinstance(granted, list):
        granted = ','.join(granted)
    require(sorted(s.strip() for s in granted.split(',') if s.strip()) == sorted(expected), 'slack_scopes_differ_from_manifest')
    for name, call in (('users_list', lambda: client.users_list(limit=1)),
                       ('conversations_list', lambda: client.conversations_list(types='public_channel', limit=1)),
                       ('conversations_open', lambda: client.conversations_open(users=NOBODY))):
        try:
            call()
        except Exception as error:
            require(slack_error(error) == 'missing_scope', f'slack_{name}_not_refused')
        else:
            raise Failure(f'slack_{name}_not_refused')
    check_boundary(client, boundary)
    member = set()
    for page in range(1, 101):
        listing = client.files_list(count=100, page=page)
        for item in listing.get('files', []):
            for channel in (*item.get('channels', []), *item.get('groups', []), *item.get('ims', [])):
                if channel in member:
                    continue
                try:
                    client.conversations_history(channel=channel, limit=1)
                except Exception as error:
                    if slack_error(error) in REFUSED:
                        raise Failure('slack_file_visible_outside_membership') from None
                    raise Failure('slack_boundary_check_incomplete') from None
                member.add(channel)
        if page >= (listing.get('paging') or {}).get('pages', 1):
            return
    raise Failure('slack_boundary_check_incomplete')


def probe(client, request):
    require(request['home']['scope_id'], 'slack_workspace_unknown')
    auth = client.auth_test()
    require(auth.get('ok') and auth.get('bot_id'), 'slack_bot_identity_failed')
    require(auth.get('team_id') == request['home']['scope_id'], 'wrong_slack_workspace')
    socket = client.apps_connections_open(app_token=request['tokens']['SLACK_APP_TOKEN'])
    require(socket.get('ok') and socket.get('url'), 'slack_app_authentication_failed')
    return auth


def only_slack(root=HOME):
    import hermes_yaml as yaml  # Hermes's own YAML reader; its environment has no PyYAML
    config = yaml.safe_load((root / 'config.yaml').read_text())
    enabled = [name for name, value in (config.get('platforms') or {}).items() if (value or {}).get('enabled')]
    require(enabled == ['slack'], 'slack_not_the_only_channel')


def continuity_channels(client, allowed):
    """Existing, unarchived DMs with allowlisted members only."""
    channels, cursor = [], None
    for _ in range(20):
        page = client.conversations_list(types='im', limit=200, cursor=cursor)
        channels.extend(c['id'] for c in page['channels']
                        if c.get('user') in allowed and c['id'].startswith('D') and not c.get('is_archived'))
        cursor = (page.get('response_metadata') or {}).get('next_cursor')
        if not cursor:
            return list(dict.fromkeys(channels))
    raise Failure('slack_dm_pagination_incomplete')


def continuity(request, client, root=HOME):
    """A fresh message from an allowlisted member in an existing DM, answered by the bot after it."""
    allowed = check_live_slack(request['boundary'], root)
    require(request['home']['scope_id'], 'slack_workspace_unknown')
    auth = client.auth_test()
    require(auth.get('team_id') == request['home']['scope_id'], 'wrong_slack_workspace')
    bot = auth['user_id']
    for channel in continuity_channels(client, allowed):
        messages = client.conversations_history(channel=channel, oldest=f"{request['since']:.6f}", limit=100)['messages']
        asked = [m for m in messages if m.get('user') in allowed and m.get('user') != bot
                 and not m.get('subtype') and not m.get('bot_id')
                 and m.get('text', '').strip() and m.get('thread_ts', m['ts']) == m['ts']]
        for message in asked:
            replies = client.conversations_replies(channel=channel, ts=message['ts'], limit=100)['messages']
            answered = [m for m in replies + messages if m.get('user') == bot and m.get('text', '').strip()
                        and float(m['ts']) > float(message['ts'])
                        and (m.get('thread_ts') == message['ts'] or not m.get('thread_ts'))]
            if answered:
                return {'observed': True, 'request_ts': message['ts'], 'reply_ts': answered[0]['ts'], 'channel': channel}
    return {'observed': False}


def setup(request, client, root=HOME, probes=True):
    """probes is False for a move: the app and tokens are the ones already checked at create."""
    from gateway.status import get_running_pid
    require(get_running_pid() is None, 'gateway_started_before_initialization')
    only_slack(root)
    check_live_slack(request['boundary'], root)
    atomic(root / '.env', slack_env((root / '.env').read_text(), request['tokens']).encode())
    auth = client.auth_test()
    # No workspace on record (an agent troupe created from a published kit, set up the first
    # time): the bot token's own team becomes the agent's, recorded by the host and checked from
    # then on.
    if not request['home']['scope_id']:
        require(re.fullmatch(r'T[A-Z0-9]+', auth.get('team_id') or ''), 'slack_workspace_or_bot_mismatch')
        request['home']['scope_id'] = auth['team_id']
    require(auth.get('ok') and auth.get('bot_id') and auth.get('team_id') == request['home']['scope_id'],
            'slack_workspace_or_bot_mismatch')
    if probes:
        check_scopes(client, auth, request['scopes'], request['boundary'])
    # The bot token was just authenticated above; only the app token remains.
    socket = client.apps_connections_open(app_token=request['tokens']['SLACK_APP_TOKEN'])
    require(socket.get('ok') and socket.get('url'), 'slack_app_authentication_failed')
    return request['home']['scope_id']


def main(argv):
    if len(argv) != 1 or argv[0] not in ('setup', 'move', 'probe', 'channel', 'continuity'):
        print("usage: slack setup|move|probe|channel|continuity", file=sys.stderr)
        return 2
    if argv[0] == 'channel':
        try:
            only_slack()
        except Exception as error:
            print(json.dumps({"status": "failed", "class": failure_class(error)}))
            return 1
        print(json.dumps({"status": "passed"}))
        return 0
    request = json.loads((Path(__file__).resolve().parent / 'request.json').read_text())
    try:
        require(os.getuid() == 1000, 'wrong_guest_user')
        tokens = request['tokens']
        require(set(tokens) == {'SLACK_BOT_TOKEN', 'SLACK_APP_TOKEN'}, 'slack_tokens_not_declared')
        from slack_sdk import WebClient
        client = WebClient(token=tokens['SLACK_BOT_TOKEN'], timeout=20)
        if argv[0] in ('setup', 'move'):
            print(json.dumps({"status": "passed", "team": setup(request, client, probes=argv[0] == 'setup')}))
            return 0
        elif argv[0] == 'continuity':
            print(json.dumps({"status": "passed", **continuity(request, client)}))
            return 0
        else:
            probe(client, request)
    except Exception as error:
        print(json.dumps({"status": "failed", "class": failure_class(error)}))
        return 1
    print(json.dumps({"status": "passed"}))
    return 0
