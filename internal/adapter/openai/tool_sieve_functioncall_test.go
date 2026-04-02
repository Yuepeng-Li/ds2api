package openai

import "testing"

func TestFindQuotedFunctionCallKeyStart_PrefersEarlierBareKey(t *testing.T) {
	input := `{functionCall:{"name":"a","arguments":"{}"},"message":"literal text: \"functionCall\": not a key"}`

	got := findQuotedFunctionCallKeyStart(input)
	want := 1
	if got != want {
		t.Fatalf("findQuotedFunctionCallKeyStart() = %d, want %d", got, want)
	}
}

func TestFindQuotedFunctionCallKeyStart_PrefersEarlierQuotedKey(t *testing.T) {
	input := `{"functionCall":{"name":"a","arguments":"{}"},"note":"functionCall appears in prose"}`

	got := findQuotedFunctionCallKeyStart(input)
	want := 1
	if got != want {
		t.Fatalf("findQuotedFunctionCallKeyStart() = %d, want %d", got, want)
	}
}
