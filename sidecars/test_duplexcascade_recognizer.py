import unittest
from duplexcascade_recognizer import append_only_delta


class AdmissionTests(unittest.TestCase):
    def test_final_word_is_admitted_before_completed_word_marker(self):
        previous, received = '', []
        for text in ['What is', 'What is the capital of', 'What is the capital of Fran', 'What is the capital of France?']:
            received.append(append_only_delta(previous, {'text':text,'stable_text':'What is','stability':'decoder-append-only'}))
            previous = text
        self.assertEqual(''.join(received), 'What is the capital of France?')
        self.assertEqual(received[-1], 'ce?')

    def test_unverified_or_revised_text_is_rejected(self):
        with self.assertRaises(ValueError):
            append_only_delta('', {'text':'test','stability':'heuristic'})
        with self.assertRaises(ValueError):
            append_only_delta('Paris', {'text':'London','stability':'decoder-append-only'})


if __name__ == '__main__': unittest.main()
