package dataplane

import (
	"context"
	"testing"
)

func TestFlowtableOffloadRejectsMeteredRuntime(t *testing.T) {
	for _, raw := range []string{`{"nftables":[{"flowtable":{"family":"inet","table":"external","name":"fast"}}]}`, `{"nftables":[{"rule":{"expr":[{"flow":{"op":"add","flowtable":"fast"}}]}}]}`, `{}`} {
		if e := CheckMeteringEnvironment(context.Background(), counterRunner{raw: raw}); e == nil {
			t.Fatal("accepted offload or malformed global inventory", raw)
		}
	}
	if e := CheckMeteringEnvironment(context.Background(), counterRunner{raw: `{"nftables":[{"table":{"family":"ip","name":"external"}}]}`}); e != nil {
		t.Fatal("ordinary external firewall rejected", e)
	}
}
