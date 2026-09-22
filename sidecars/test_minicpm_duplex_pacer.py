"""Cancellation at the pacing wait must suppress the pending audio packet."""
import io
import threading
import unittest
from unittest.mock import patch

from minicpm_o_duplex_sidecar import MiniCPMODuplexSidecar, MODEL_OUTPUT_RATE


class PacerTests(unittest.TestCase):
    def test_control_during_wait_drops_pending_packet(self):
        for control in ('interrupt', 'close'):
            with self.subTest(control=control):
                sidecar = MiniCPMODuplexSidecar(
                    io.BytesIO(), io.BytesIO(), host=None, mock=False,
                    idle_fill_ms=0, use_hello_instructions=False, busy_timeout=0,
                    metrics_log=None, length_penalty=1.05, force_listen_count=3,
                    playout_lead_ms=0)
                sent = []
                sidecar.audio = sent.append
                packet = b'\x01\x00' * int(MODEL_OUTPUT_RATE * .08)
                sidecar._queue_out('audio', packet * 2)
                waited = threading.Event()

                def during_wait(_):
                    if control == 'interrupt':
                        sidecar.on_interrupt()
                    else:
                        sidecar._stop.set()
                    waited.set()

                with patch('minicpm_o_duplex_sidecar.time.sleep', during_wait):
                    worker = threading.Thread(target=sidecar._pacer)
                    worker.start()
                    self.assertTrue(waited.wait(1))
                    sidecar._stop.set()
                    worker.join(1)
                self.assertFalse(worker.is_alive())
                self.assertEqual(sent, [packet])


if __name__ == '__main__':
    unittest.main()
