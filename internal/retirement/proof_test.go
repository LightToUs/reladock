package retirement

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func proofInput() ProofInput {
	return ProofInput{
		TaskID: "11111111-1111-4111-8111-111111111111", AllocationID: "22222222-2222-4222-8222-222222222222", NodeID: "hk-01",
		Address: "192.0.2.10", Protocol: "tcp", Port: 20001, QuarantineGeneration: 7,
		GuardBeforeDigest: strings.Repeat("a", 64), GuardAfterDigest: strings.Repeat("b", 64), ConntrackVersion: "conntrack v1.4.7 (conntrack-tools)",
		DeletedEntries: 2, KernelBootID: "33333333-3333-4333-8333-333333333333", JournalSequence: 9, GoodDigest: strings.Repeat("c", 64),
		ObservedAt: time.Date(2026, 11, 1, 3, 4, 5, 600, time.FixedZone("test", 7*3600)),
	}
}

func TestBuildProofIsCanonicalBoundedAndStable(t *testing.T) {
	proof, raw, digest, err := BuildProof(proofInput())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schemaVersion":1,"taskId":"11111111-1111-4111-8111-111111111111","allocationId":"22222222-2222-4222-8222-222222222222","nodeId":"hk-01","address":"192.0.2.10","protocol":"tcp","port":20001,"quarantineGeneration":7,"guardBeforeDigest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","guardAfterDigest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","conntrackVersion":"conntrack v1.4.7 (conntrack-tools)","filterVersion":"reladock-conntrack-v1","deletedEntries":2,"zeroMatches":true,"forwardGuardRemoved":true,"kernelBootId":"33333333-3333-4333-8333-333333333333","journalSequence":9,"goodDigest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","observedAt":"2026-10-31T20:04:05.0000006Z"}`
	if string(raw) != want || digest != "cb5374842e5c30bf379cfedcee59dd8be5babaa630478fa178238ab14a60fda1" {
		t.Fatalf("noncanonical proof\n%s\n%s", raw, digest)
	}
	if !proof.ZeroMatches || !proof.ForwardGuardRemoved || proof.FilterVersion != "reladock-conntrack-v1" || proof.ObservedAt.Location() != time.UTC {
		t.Fatalf("proof flags/time not normalized: %+v", proof)
	}
	var decoded map[string]any
	if json.Unmarshal(raw, &decoded) != nil || len(raw) > MaxProofBytes {
		t.Fatal("proof is not bounded JSON")
	}
}

func TestBuildProofRejectsUnboundOrUnsafeValues(t *testing.T) {
	cases := map[string]func(*ProofInput){
		"task":         func(v *ProofInput) { v.TaskID = "not-a-uuid" },
		"allocation":   func(v *ProofInput) { v.AllocationID = "not-a-uuid" },
		"node":         func(v *ProofInput) { v.NodeID = "bad node" },
		"address":      func(v *ProofInput) { v.Address = "0.0.0.0" },
		"protocol":     func(v *ProofInput) { v.Protocol = "both" },
		"port":         func(v *ProofInput) { v.Port = 1 },
		"generation":   func(v *ProofInput) { v.QuarantineGeneration = 0 },
		"guard digest": func(v *ProofInput) { v.GuardBeforeDigest = "secret" },
		"version":      func(v *ProofInput) { v.ConntrackVersion = strings.Repeat("x", 81) },
		"deleted":      func(v *ProofInput) { v.DeletedEntries = -1 },
		"boot":         func(v *ProofInput) { v.KernelBootID = "bad" },
		"sequence":     func(v *ProofInput) { v.JournalSequence = 0 },
		"good digest":  func(v *ProofInput) { v.GoodDigest = "bad" },
		"time":         func(v *ProofInput) { v.ObservedAt = time.Time{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input := proofInput()
			mutate(&input)
			if _, raw, digest, err := BuildProof(input); err == nil || raw != nil || digest != "" {
				t.Fatalf("unsafe proof accepted: raw=%s digest=%s err=%v", raw, digest, err)
			}
		})
	}
}
