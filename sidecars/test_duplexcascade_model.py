"""Verify native control/text ordering and independent conversation state."""
import unittest
from types import SimpleNamespace
from duplexcascade_model import DuplexCascadeSession


class Tokenizer:
    bos_token_id = 99
    names = ['<|user is talking|>', '<|user finish talking|>', '<|user is thinking|>',
             '<|user interruption|>', '<|user backchannel|>', '<|no voice|>', '<|im_end|>']
    def convert_tokens_to_ids(self, text):
        return self.names.index(text) + 100
    def decode(self, tokens):
        return bytes(tokens).decode('utf-8')


class SessionTests(unittest.TestCase):
    def test_control_precedes_text_and_multibyte_text_is_not_split(self):
        session = DuplexCascadeSession(SimpleNamespace(tokenizer=Tokenizer()))
        tokens = [103, 101, *'你好'.encode(), 106]
        self.assertEqual(list(session.events(tokens)), [
            ('control', '<|user interruption|>'), ('control', '<|user finish talking|>'),
            ('text', '你好'), ('control', '<|im_end|>')])

    def test_sessions_do_not_share_mutable_history(self):
        backend = SimpleNamespace(tokenizer=Tokenizer())
        first, second = DuplexCascadeSession(backend), DuplexCascadeSession(backend)
        first.history.append(5)
        self.assertEqual(second.history, [99])


if __name__ == '__main__':
    unittest.main()
