#!/usr/bin/env python3
"""For each guard in the federated-join path, remove it and check that a test fails.

A test that passes with the guard removed is not testing the guard, whatever it says.
"""
import os
import re
import shutil
import subprocess
import sys

ROOT = os.path.expanduser('~/Projects/qwatrix')
JOINF = 'internal/fedapi/joinremote.go'
RSF = 'internal/roomserver/joinremote.go'
BACKUPS = {f: '/tmp/gp-' + os.path.basename(f) for f in (JOINF, RSF)}

CASES = [
    ('event from another room', JOINF,
     'if declared := event.RoomID().String(); declared != roomID {',
     'if declared := event.RoomID().String(); false && declared != roomID {',
     'TestAFederatedJoinRefusesStateFromAnotherRoom'),
    ('unsigned event', JOINF,
     'if err := gomatrixserverlib.VerifyEventSignatures(ctx, event, c.opts.KeyRing, userIDForSender); err != nil {',
     'if false {',
     'TestAFederatedJoinRefusesUnsignedState'),
    ('peer template naming somebody else', JOINF,
     'if err := checkJoinTemplate(proto, userID, roomID); err != nil {\n\t\treturn nil, err\n\t}',
     '_ = checkJoinTemplate(proto, userID, roomID)',
     'TestAFederatedJoinRefusesATemplateForSomebodyElse'),
    ('state contradicting itself', JOINF,
     'if seenStateKey[key] {',
     'if false && seenStateKey[key] {',
     'TestAFederatedJoinRefusesContradictoryState'),
    ('answer joining a different room', RSF,
     'if joined := remote.JoinEvent.RoomID().String(); joined != roomID {',
     'if joined := remote.JoinEvent.RoomID().String(); false && joined != roomID {',
     'TestAFederatedJoinRefusesStateFromAnotherRoom'),
    ('room rules admitting the join', RSF,
     'if err := s.events.Authorize(remote.JoinEvent, prior); err != nil {\n\t\treturn "", err\n\t}',
     '_ = s.events.Authorize(remote.JoinEvent, prior)',
     'TestAFederatedJoinRefusesAPrivateRoom'),
    ('incomplete auth chain', RSF,
     'if err := s.requireCompleteAuthChain(remote.JoinEvent, prior); err != nil {\n\t\treturn "", err\n\t}',
     '_ = s.requireCompleteAuthChain(remote.JoinEvent, prior)',
     'TestAFederatedJoinRefusesATemplateWithAnIncompleteChain'),
    ('answer without the join it accepted', RSF,
     'if len(prior) == len(state) {',
     'if false && len(prior) == len(state) {',
     'TestAFederatedJoinRefusesAnAnswerWithoutTheJoinItAccepted'),
    ('two member events for the joiner', JOINF,
     'if stateKey := event.StateKey(); stateKey != nil && event.EventID() != joinEvent.EventID() {',
     'if stateKey := event.StateKey(); stateKey != nil {',
     'TestAFederatedJoinOfARejoiningUser'),
]


def run(test):
    proc = subprocess.run(['go', 'test', './tests/api/', '-run', test, '-count=1', '-v'],
                          cwd=ROOT, capture_output=True, text=True, timeout=400)
    out = proc.stdout + proc.stderr
    ran = bool(re.search(r'^=== RUN\s+' + test, out, re.M))
    return ran, bool(re.search(r'^--- FAIL: ' + test, out, re.M))


def main():
    for f, backup in BACKUPS.items():
        shutil.copy(os.path.join(ROOT, f), backup)
    failures = 0
    try:
        for name, path, old, new, test in CASES:
            full = os.path.join(ROOT, path)
            src = open(full).read()
            if src.count(old) != 1:
                print('  %-38s PATTERN MATCHED %d TIMES' % (name, src.count(old)))
                failures += 1
                continue
            open(full, 'w').write(src.replace(old, new))
            if subprocess.run(['go', 'build', './...'], cwd=ROOT,
                              capture_output=True, text=True).returncode != 0:
                print('  %-38s guard cannot be removed on its own (build error)' % name)
                failures += 1
            else:
                ran, failed = run(test)
                if not ran:
                    print('  %-38s TEST DID NOT RUN' % name)
                    failures += 1
                elif failed:
                    print('  %-38s fails without the guard, as it should' % name)
                else:
                    print('  %-38s STILL PASSES — the test does not pin this guard' % name)
                    failures += 1
            shutil.copy(BACKUPS[path], full)
    finally:
        for f, backup in BACKUPS.items():
            shutil.copy(backup, os.path.join(ROOT, f))
        subprocess.run(['gofmt', '-w', 'internal/fedapi', 'internal/roomserver'], cwd=ROOT,
                       capture_output=True)
    build = subprocess.run(['go', 'build', './...'], cwd=ROOT, capture_output=True, text=True)
    print('restored:', 'clean' if build.returncode == 0 else build.stderr[:200])
    return 1 if failures else 0


sys.exit(main())
