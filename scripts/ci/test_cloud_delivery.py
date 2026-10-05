"""Exercise delivery boundaries against real disposable Git repositories."""

import contextlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location('cloud_delivery', Path(__file__).with_name('cloud_delivery.py'))
delivery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(delivery)


class DeliveryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name) / 'repo'
        self.repo.mkdir()
        self.git('init', '-b', 'main')
        self.git('config', 'user.email', 'test@example.invalid')
        self.git('config', 'user.name', 'Delivery Test')
        self.git('remote', 'add', 'origin', 'https://example.invalid/repo.git')
        (self.repo / 'source.txt').write_text('original\n')
        self.git('add', '.')
        self.git('commit', '-m', 'test: initial')
        self.git('switch', '-c', 'codex/delivery-1-test')
        self.state = {'phase': 'implemented', 'repo': str(self.repo), 'issue': 1,
                      'branch': 'codex/delivery-1-test', 'base': 'main',
                      'base_sha': self.git('rev-parse', 'HEAD'), 'title': 'Test task',
                      'origin': 'https://example.invalid/repo.git', 'gates': []}
        self.run = Path(self.temp.name) / 'run'
        self.run.mkdir()
        self.path = self.run / 'state.json'
        delivery.save(self.path, self.state)

    def git(self, *args):
        return subprocess.check_output(['git', *args], cwd=self.repo, stderr=subprocess.DEVNULL, text=True).strip()

    def install_hooks(self):
        hooks = self.repo / 'scripts/git-hooks'
        hooks.mkdir(parents=True)
        for name in ['pre-commit', 'pre-push', 'commit-msg']:
            p = hooks / name
            p.write_text('#!/bin/sh\nexit 0\n')
            p.chmod(0o755)
        self.git('config', 'core.hooksPath', 'scripts/git-hooks')

    def certify(self):
        self.state.update(phase='validated', validated_fingerprint=delivery.fingerprint(self.repo))
        delivery.save(self.path, self.state)

    def test_dirty_preparation_never_contacts_github_or_switches_branch(self):
        (self.repo / 'new.txt').write_text('preserve me')
        with patch.object(delivery.shutil, 'which') as which:
            with self.assertRaisesRegex(delivery.DeliveryError, 'clean checkout'):
                delivery.prepare(self.repo, self.run, 1, None)
        which.assert_not_called()
        self.assertEqual(self.git('branch', '--show-current'), self.state['branch'])

    def test_fingerprint_detects_untracked_deleted_and_mode_changes(self):
        before = delivery.fingerprint(self.repo)
        path = self.repo / 'source.txt'
        original_mode = path.stat().st_mode & 0o777
        path.chmod(0o755)
        self.assertNotEqual(before, delivery.fingerprint(self.repo))
        path.chmod(original_mode)
        self.assertEqual(before, delivery.fingerprint(self.repo))
        extra = self.repo / 'extra.txt'
        extra.write_text('new')
        self.assertNotEqual(before, delivery.fingerprint(self.repo))
        extra.unlink()
        path.unlink()
        self.assertNotEqual(before, delivery.fingerprint(self.repo))

    def test_failed_gate_cannot_publish_and_later_gates_do_not_run(self):
        commands = [('first', ['false']), ('later', ['true'])]
        with patch.object(delivery, 'gate_commands', return_value=commands):
            with patch.object(delivery, 'bounded_command', side_effect=delivery.DeliveryError('failure')) as command:
                with self.assertRaises(delivery.DeliveryError):
                    delivery.validate(self.repo, self.path, self.state, 2)
        self.assertEqual(command.call_count, 1)
        self.assertNotIn('validated_fingerprint', self.state)
        with patch.object(delivery, 'bounded_command') as publish:
            with self.assertRaisesRegex(delivery.DeliveryError, 'quality gates'):
                delivery.publish(self.repo, self.path, self.state, 2)
        publish.assert_not_called()

    def test_mutation_during_validation_requires_revalidation(self):
        def mutate(*args, **kwargs):
            (self.repo / 'source.txt').write_text('changed by gate')
        with patch.object(delivery, 'gate_commands', return_value=[('mutating', ['true'])]):
            with patch.object(delivery, 'bounded_command', side_effect=mutate):
                with self.assertRaisesRegex(delivery.DeliveryError, 'changed source'):
                    delivery.validate(self.repo, self.path, self.state, 2)
        self.assertNotIn('validated_fingerprint', self.state)

    def test_changed_code_after_validation_never_publishes(self):
        self.install_hooks()
        self.certify()
        (self.repo / 'source.txt').write_text('late edit')
        with patch.object(delivery, 'bounded_command') as command:
            with self.assertRaisesRegex(delivery.DeliveryError, 'changed after validation'):
                delivery.publish(self.repo, self.path, self.state, 2)
        command.assert_not_called()

    def test_wrong_branch_and_remote_are_rejected(self):
        self.git('switch', 'main')
        with self.assertRaisesRegex(delivery.DeliveryError, 'branch differs'):
            delivery.branch_guard(self.repo, self.state)
        self.git('switch', self.state['branch'])
        self.git('remote', 'set-url', 'origin', 'https://example.invalid/other.git')
        with self.assertRaisesRegex(delivery.DeliveryError, 'Origin changed'):
            delivery.branch_guard(self.repo, self.state)

    def test_missing_hooks_block_publication(self):
        self.certify()
        with self.assertRaises(delivery.DeliveryError):
            delivery.require_validated(self.repo, self.state)

    def test_second_runner_cannot_lock_checkout(self):
        with delivery.checkout_lock(self.repo):
            with self.assertRaisesRegex(delivery.DeliveryError, 'Another delivery'):
                with delivery.checkout_lock(self.repo):
                    self.fail('Lock unexpectedly acquired')

    def test_successful_validation_is_recorded_and_can_be_rechecked(self):
        self.install_hooks()
        with patch.object(delivery, 'gate_commands', return_value=[('gate', ['true'])]):
            with patch.object(delivery, 'bounded_command'):
                delivery.validate(self.repo, self.path, self.state, 2)
        delivery.require_validated(self.repo, self.state)
        recorded = json.loads(self.path.read_text())
        self.assertEqual(recorded['phase'], 'validated')
        self.assertEqual(recorded['gates'][0]['status'], 'passed')

    def test_published_resume_reuses_existing_pr_without_push(self):
        self.install_hooks()
        self.certify()
        self.state['phase'] = 'published'
        real_capture = delivery.capture
        def fake_capture(repo, args):
            if args[:3] == ['gh', 'pr', 'list']:
                return '[{"url":"https://example.invalid/pr/1"}]'
            return real_capture(repo, args)
        with patch.object(delivery, 'capture', side_effect=fake_capture):
            with patch.object(delivery, 'bounded_command') as command:
                with contextlib.redirect_stdout(io.StringIO()):
                    delivery.publish(self.repo, self.path, self.state, 2)
        command.assert_not_called()
        self.assertEqual(self.state['pr_url'], 'https://example.invalid/pr/1')

    def test_real_commit_and_push_create_draft_pr_with_issue_and_body_file(self):
        self.install_hooks()
        remote = Path(self.temp.name) / 'remote.git'
        subprocess.run(['git', 'init', '--bare', str(remote)], check=True, capture_output=True)
        self.git('remote', 'set-url', 'origin', str(remote))
        self.state['origin'] = str(remote)
        self.certify()
        real_capture = delivery.capture
        created = []
        def fake_capture(repo, args):
            if args[:3] == ['gh', 'pr', 'list']:
                return '[]'
            if args[:3] == ['gh', 'pr', 'create']:
                created.append(args)
                return 'https://example.invalid/pr/2'
            return real_capture(repo, args)
        with patch.object(delivery, 'capture', side_effect=fake_capture):
            with contextlib.redirect_stdout(io.StringIO()):
                delivery.publish(self.repo, self.path, self.state, 10)
        self.assertEqual(self.state['phase'], 'published')
        self.assertEqual(self.git('status', '--porcelain'), '')
        remote_sha = subprocess.check_output(
            ['git', '--git-dir', str(remote), 'rev-parse', 'refs/heads/' + self.state['branch']], text=True
        ).strip()
        self.assertEqual(remote_sha, self.state['commit'])
        self.assertIn('--draft', created[0])
        self.assertIn('--body-file', created[0])
        self.assertIn('Refs #1', (self.run / 'pr-body.md').read_text())

    def test_commit_hook_mutation_cannot_reach_push(self):
        self.install_hooks()
        hook = self.repo / 'scripts/git-hooks/pre-commit'
        hook.write_text('#!/bin/sh\nprintf changed > source.txt\ngit add source.txt\n')
        hook.chmod(0o755)
        self.certify()
        with contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(delivery.DeliveryError, 'changed validated contents'):
                delivery.publish(self.repo, self.path, self.state, 10)
        self.assertFalse((self.run / 'push.log').exists())

    def test_timeout_stops_hung_command_and_preserves_log(self):
        with contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(delivery.DeliveryError, 'timed out'):
                delivery.bounded_command(
                    self.repo, [sys.executable, '-c', 'import signal; signal.pause()'],
                    self.run / 'timeout.log', 1,
                )
        self.assertTrue((self.run / 'timeout.log').is_file())


if __name__ == '__main__':
    unittest.main()
