package bundle

import "testing"

func TestRemoveSourceIsCaseInsensitive(t *testing.T) {
	b := &Bundle{Version: Version}
	if err := b.AddSource("SmallRye"); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveSource("smallrye"); err != nil {
		t.Fatalf("RemoveSource(smallrye) with source SmallRye: %v", err)
	}
	if len(b.Sources) != 0 {
		t.Fatalf("sources = %+v, want none", b.Sources)
	}
}
