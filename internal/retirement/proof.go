package retirement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"time"

	"reladock/internal/platform"
)

const MaxProofBytes = 64 * 1024

var proofUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var proofNode = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)
var proofDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

type ProofInput struct {
	TaskID               string
	AllocationID         string
	NodeID               string
	Address              string
	Protocol             string
	Port                 int
	QuarantineGeneration int64
	GuardBeforeDigest    string
	GuardAfterDigest     string
	ConntrackVersion     string
	DeletedEntries       int
	KernelBootID         string
	JournalSequence      int64
	GoodDigest           string
	ObservedAt           time.Time
}

func BuildProof(input ProofInput) (platform.RetirementProof, []byte, string, error) {
	address, err := netip.ParseAddr(input.Address)
	if err != nil || !address.Is4() || !address.IsGlobalUnicast() || address.IsLoopback() || address.String() != input.Address ||
		!proofUUID.MatchString(input.TaskID) || !proofUUID.MatchString(input.AllocationID) || !proofNode.MatchString(input.NodeID) ||
		(input.Protocol != "tcp" && input.Protocol != "udp") || input.Port < 20000 || input.Port > 60000 || input.QuarantineGeneration < 1 ||
		!proofDigest.MatchString(input.GuardBeforeDigest) || !proofDigest.MatchString(input.GuardAfterDigest) || len(input.ConntrackVersion) < 1 || len(input.ConntrackVersion) > 80 ||
		input.DeletedEntries < 0 || !proofUUID.MatchString(input.KernelBootID) || input.JournalSequence < 1 || !proofDigest.MatchString(input.GoodDigest) || input.ObservedAt.IsZero() {
		return platform.RetirementProof{}, nil, "", errors.New("invalid retirement proof input")
	}
	proof := platform.RetirementProof{
		SchemaVersion:        1,
		TaskID:               input.TaskID,
		AllocationID:         input.AllocationID,
		NodeID:               input.NodeID,
		Address:              address.String(),
		Protocol:             input.Protocol,
		Port:                 input.Port,
		QuarantineGeneration: input.QuarantineGeneration,
		GuardBeforeDigest:    input.GuardBeforeDigest,
		GuardAfterDigest:     input.GuardAfterDigest,
		ConntrackVersion:     input.ConntrackVersion,
		FilterVersion:        "reladock-conntrack-v1",
		DeletedEntries:       input.DeletedEntries,
		ZeroMatches:          true,
		ForwardGuardRemoved:  true,
		KernelBootID:         input.KernelBootID,
		JournalSequence:      input.JournalSequence,
		GoodDigest:           input.GoodDigest,
		ObservedAt:           input.ObservedAt.UTC(),
	}
	raw, err := json.Marshal(proof)
	if err != nil || len(raw) > MaxProofBytes {
		return platform.RetirementProof{}, nil, "", errors.New("invalid retirement proof encoding")
	}
	digest := sha256.Sum256(raw)
	return proof, raw, hex.EncodeToString(digest[:]), nil
}
