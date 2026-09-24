import importlib.util
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).parent))
spec = importlib.util.spec_from_file_location('campaign_report', Path(__file__).with_name('campaign_report.py'))
campaign_report = importlib.util.module_from_spec(spec)
spec.loader.exec_module(campaign_report)


def row(branch, silent, withheld, passed):
    return {'branch': branch, 'silent_expectation': silent, 'feedback_withheld': withheld, 'screen_passed': passed}


class SilentVerdictTest(unittest.TestCase):
    def test_silence_counts_only_when_the_control_speaks(self):
        cases = {(True, False): 'feedback-only-pass', (True, True): 'non-discriminating',
                 (False, False): 'no-feedback-pass', (False, True): 'non-discriminating'}
        for (feedback, control), verdict in cases.items():
            rows = [row('mid', True, False, feedback), row('mid-nofeedback', True, True, control),
                    row('done', False, False, True), row('done-nofeedback', False, True, False)]
            out = campaign_report.silent_verdicts(rows)
            self.assertEqual([o['verdict'] for o in out], [verdict])

    def test_missing_control_is_incomplete(self):
        out = campaign_report.silent_verdicts([row('mid', True, False, True)])
        self.assertEqual(out[0]['verdict'], 'incomplete-pair')


class OpportunityTest(unittest.TestCase):
    def test_heard_and_speaking_at_onset(self):
        segs = [{'text': 'done', 'first_played_at': 1, 'last_played_at': 4, 'completed_at': 4},
                {'text': 'across', 'first_played_at': 8, 'last_played_at': 12, 'completed_at': 12},
                {'text': 'later', 'first_played_at': 15, 'last_played_at': 18, 'completed_at': 18}]
        self.assertEqual(campaign_report.opportunity(segs, 10),
                         {'feedback_onset_ns': 10, 'heard_before_onset': ['done'], 'speaking_at_onset': True})
        self.assertFalse(campaign_report.opportunity(segs, 13)['speaking_at_onset'])
        self.assertEqual(campaign_report.opportunity([], 5)['heard_before_onset'], [])


class TreatmentTest(unittest.TestCase):
    def test_treatment_comes_from_the_campaign_run_name(self):
        self.assertEqual(campaign_report.treatment_of('/x/A2-v4-sc-01'), 'A2-v4')
        self.assertEqual(campaign_report.treatment_of('/x/A1-v4-sc-01'), 'A1-v4')
        self.assertEqual(campaign_report.treatment_of('/x/A2D-v4-sc-01'), 'A2D-v4')
        self.assertEqual(campaign_report.treatment_of('/x/qwen-run'), 'unlabelled')
        self.assertEqual(campaign_report.treatment_of('/x/fresh-st02-gap10-v4-20260923-02'), 'unlabelled')


if __name__ == '__main__':
    unittest.main()
