package scenario

import "testing"

// A voice that translates as the words arrive ends each piece where the
// recogniser's partial did; the phrase it said across two pieces is still
// the phrase. Punctuation the check asks for stays literal.
func TestContainsPhraseAcrossThePunctuationBetweenPieces(t *testing.T) {
	if !containsPhrase("hello, very pleased. to meet you.", "pleased to meet") {
		t.Fatal("a phrase said across two pieces was not found")
	}
	if containsPhrase("hello, very pleased. to meet you.", "pleased to meet?") {
		t.Fatal("a phrase with its own punctuation must stay literal")
	}
	if containsPhrase("is it done. yet", "?") {
		t.Fatal("a punctuation check must stay literal")
	}
	if containsPhrase("the undone work", "done") {
		t.Fatal("a word inside another word is still not the word")
	}
}
