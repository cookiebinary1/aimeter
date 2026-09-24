"""Exercise the installer offline with real archives and mocked release downloads."""
import hashlib
import io
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tarfile
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'install.sh'
BINARY = b'#!/bin/sh\nprintf "aimeter test binary\\n"\n'


@unittest.skipIf(os.name == 'nt', 'The shell installer targets macOS and Linux')
class InstallerTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='aimeter installer test ')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / 'tools'
        self.bin.mkdir()
        self.dest = self.root / 'install directory'
        self.dest.mkdir()
        self.target = self.dest / 'aimeter'
        self.target.write_bytes(b'existing binary')
        self.downloads = self.root / 'releases'
        self.downloads.mkdir()
        self.tmp = self.root / 'tmp'
        self.tmp.mkdir()
        self.env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ['PATH'],
                        AIMETER_INSTALL_DIR=str(self.dest), AIMETER_VERSION='v1.2.3',
                        TMPDIR=str(self.tmp), TEST_ROOT=str(self.root),
                        TEST_OS='Linux', TEST_ARCH='x86_64')
        self.mock('uname', '''import os, sys
print(os.environ['TEST_OS' if sys.argv[1] == '-s' else 'TEST_ARCH'])
''')
        self.mock('curl', '''import os, shutil, sys
from pathlib import Path
args = sys.argv[1:]
url = args[-1]
root = Path(os.environ['TEST_ROOT'])
with (root / 'requests').open('a') as log: log.write(url + '\\n')
assert url.startswith('https://github.com/cookiebinary1/aimeter/releases/')
assert '--proto' in args and '--proto-redir' in args
if os.getenv('TEST_DOWNLOAD_FAIL'): sys.exit(22)
if url.endswith('/latest'):
    print(os.getenv('TEST_LATEST_URL', 'https://github.com/cookiebinary1/aimeter/releases/tag/v1.2.3'), end='')
else:
    shutil.copyfile(root / 'releases' / url.rsplit('/', 1)[-1], args[args.index('--output') + 1])
''')
        self.archive('linux', 'amd64')

    def mock(self, name, code):
        path = self.bin / name
        # Invoke the same Python even if its path contains spaces.
        runner = self.bin / (name + '.py')
        runner.write_text(code)
        path.write_text('#!/bin/sh\nexec ' + shlex.quote(sys.executable) + ' ' + shlex.quote(str(runner)) + ' "$@"\n')
        path.chmod(0o755)

    def archive(self, os_name, arch, member='aimeter', symlink=False):
        name = f'aimeter_1.2.3_{os_name}_{arch}.tar.gz'
        path = self.downloads / name
        with tarfile.open(path, 'w:gz') as archive:
            entry = tarfile.TarInfo(member)
            if symlink:
                entry.type = tarfile.SYMTYPE
                entry.linkname = str(self.target)
                archive.addfile(entry)
            else:
                entry.size = len(BINARY)
                archive.addfile(entry, io.BytesIO(BINARY))
        self.checksum = hashlib.sha256(path.read_bytes()).hexdigest()
        self.archive_name = name
        (self.downloads / 'checksums.txt').write_text(f'{self.checksum}  {name}\n')

    def run_installer(self, success=True):
        result = subprocess.run(['sh', str(SCRIPT)], env=self.env, capture_output=True, text=True)
        if success:
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(self.target.read_bytes(), BINARY)
            self.assertEqual(self.target.stat().st_mode & 0o777, 0o755)
        else:
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(self.target.read_bytes(), b'existing binary')
        self.assertEqual(list(self.tmp.iterdir()), [], 'Temporary files must be removed')
        self.assertEqual(list(self.dest.glob('.aimeter.*')), [], 'Staged files must be removed')
        return result

    def test_platforms_and_architectures(self):
        for os_name, uname_os in [('linux', 'Linux'), ('darwin', 'Darwin')]:
            for arch, uname_arch in [('amd64', 'x86_64'), ('arm64', 'aarch64' if os_name == 'linux' else 'arm64')]:
                with self.subTest(os=os_name, arch=arch):
                    self.env.update(TEST_OS=uname_os, TEST_ARCH=uname_arch)
                    self.archive(os_name, arch)
                    self.run_installer()

    def test_latest_release(self):
        self.env.pop('AIMETER_VERSION')
        self.run_installer()
        self.assertIn('/releases/latest', (self.root / 'requests').read_text())

    def test_version_without_v(self):
        self.env['AIMETER_VERSION'] = '1.2.3'
        self.run_installer()

    def test_default_directory(self):
        self.env.pop('AIMETER_INSTALL_DIR')
        self.env['HOME'] = str(self.root / 'user home')
        self.dest = Path(self.env['HOME']) / '.local/bin'
        self.target = self.dest / 'aimeter'
        self.run_installer()

    def test_checksum_mismatch(self):
        (self.downloads / 'checksums.txt').write_text(f'{"0" * 64}  {self.archive_name}\n')
        self.assertIn('SHA-256 mismatch', self.run_installer(False).stderr)

    def test_missing_or_duplicate_checksum(self):
        for content in ['', f'{self.checksum}  other.tar.gz\n', (f'{self.checksum}  {self.archive_name}\n') * 2]:
            with self.subTest(content=content):
                (self.downloads / 'checksums.txt').write_text(content)
                self.run_installer(False)

    def test_failed_download(self):
        self.env['TEST_DOWNLOAD_FAIL'] = '1'
        self.run_installer(False)

    def test_unsupported_platform(self):
        self.env['TEST_OS'] = 'MINGW64_NT'
        self.run_installer(False)
        self.assertFalse((self.root / 'requests').exists())

    def test_unsupported_architecture(self):
        self.env['TEST_ARCH'] = 'riscv64'
        self.run_installer(False)
        self.assertFalse((self.root / 'requests').exists())

    def test_invalid_version(self):
        self.env['AIMETER_VERSION'] = '../../elsewhere'
        self.run_installer(False)
        self.assertFalse((self.root / 'requests').exists())

    def test_unexpected_latest_redirect(self):
        self.env['AIMETER_VERSION'] = 'latest'
        self.env['TEST_LATEST_URL'] = 'https://example.com/tag/v1.2.3'
        self.run_installer(False)

    def test_missing_binary(self):
        self.archive('linux', 'amd64', member='something-else')
        self.run_installer(False)

    def test_symlink_binary(self):
        self.archive('linux', 'amd64', symlink=True)
        self.run_installer(False)

    def test_destination_directory(self):
        self.target.unlink()
        self.target.mkdir()
        marker = self.target / 'keep'
        marker.write_text('keep')
        result = subprocess.run(['sh', str(SCRIPT)], env=self.env, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(marker.read_text(), 'keep')
        self.assertEqual(list(self.tmp.iterdir()), [])


if __name__ == '__main__':
    unittest.main()
