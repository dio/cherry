package cherry

import "testing"

func BenchmarkReaderResolveLLM(b *testing.B) {
	const principals = 100000
	blob, err := Build(testPackInput(1, principals))
	if err != nil {
		b.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}

	b.ReportAllocs()
	var found bool
	for b.Loop() {
		_, found = reader.ResolveLLM("workspace1", "slug:1:77777", "gpt-4o-mini")
	}
	if !found {
		b.Fatal("ResolveLLM() ok = false, want true")
	}
}

func BenchmarkReaderResolveLLMIDs(b *testing.B) {
	const principals = 100000
	blob, err := Build(testPackInput(1, principals))
	if err != nil {
		b.Fatalf("Build() error = %v", err)
	}
	reader, err := Open(blob)
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}

	b.ReportAllocs()
	var found bool
	var providerID uint32
	for b.Loop() {
		var ids LLMIDs
		ids, found = reader.ResolveLLMIDs("workspace1", "slug:1:77777", "gpt-4o-mini")
		providerID = ids.ProviderID
	}
	if !found || providerID != 0 {
		b.Fatalf("ResolveLLMIDs() found=%v providerID=%d, want true/0", found, providerID)
	}
}
