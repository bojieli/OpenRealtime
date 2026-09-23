import importlib.util
from pathlib import Path
import subprocess
import sys
import time
import unittest

spec = importlib.util.spec_from_file_location('campaign', Path(__file__).with_name('campaign.py'))
campaign = importlib.util.module_from_spec(spec)
spec.loader.exec_module(campaign)


class NativeDetectionTest(unittest.TestCase):
    def test_a_shell_mentioning_the_script_is_not_a_native_run(self):
        # The earlier pgrep -f check matched its own launching shell and could
        # block a campaign forever.
        shell = subprocess.Popen(['sh', '-c', 'sleep 3 # tools/interactionstudy/native_live.py'])
        try:
            time.sleep(0.2)
            self.assertFalse(campaign.native_active())
        finally:
            shell.kill()
            shell.wait()

    def test_a_python_interpreter_running_the_script_is(self):
        fake = Path(self.id().replace('.', '_'))
        directory = Path('/tmp') / fake / 'interactionstudy'
        directory.mkdir(parents=True, exist_ok=True)
        script = directory / 'native_live.py'
        script.write_text('import time\ntime.sleep(3)\n')
        # Venv interpreters appear in argv as .../bin/python.
        interpreter = directory / 'python'
        if not interpreter.exists():
            interpreter.symlink_to(sys.executable)
        process = subprocess.Popen([str(interpreter), str(script)])
        try:
            time.sleep(0.3)
            self.assertTrue(campaign.native_active())
        finally:
            process.kill()
            process.wait()


if __name__ == '__main__':
    unittest.main()
