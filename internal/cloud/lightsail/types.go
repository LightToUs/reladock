package lightsail

import (
	"context"
	"time"
)

type AvailabilityZone struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

type Blueprint struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Active   bool   `json:"active"`
}

type Bundle struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	PriceUSD           float64  `json:"priceUsd"`
	CPUCount           int32    `json:"cpuCount"`
	RAMGiB             float32  `json:"ramGib"`
	DiskGiB            int32    `json:"diskGib"`
	SupportedPlatforms []string `json:"supportedPlatforms"`
	Active             bool     `json:"active"`
}

type Catalog struct {
	Region            string             `json:"region"`
	AvailabilityZones []AvailabilityZone `json:"availabilityZones"`
	Blueprints        []Blueprint        `json:"blueprints"`
	Bundles           []Bundle           `json:"bundles"`
}

type Instance struct {
	Name             string
	ARN              string
	Region           string
	AvailabilityZone string
	BlueprintID      string
	BundleID         string
	State            string
	PublicIP         string
	Tags             map[string]string
}

type Operation struct {
	ID        string
	Status    string
	ErrorCode string
	CreatedAt time.Time
}

type StaticIP struct {
	Name             string
	IPAddress        string
	AttachedTo       string
	IsAttached       bool
	SupportCode      string
	ResourceType     string
	ResourceLocation string
}

type HostKey struct {
	Algorithm string
	PublicKey string
}

type AccessDetails struct {
	IPAddress   string
	Username    string
	PrivateKey  string
	Certificate string
	HostKeys    []HostKey
	ExpiresAt   time.Time
}

type CreateInstanceInput struct {
	Region           string
	AvailabilityZone string
	BlueprintID      string
	BundleID         string
	Name             string
	Tags             map[string]string
}

type Provider interface {
	Catalog(ctx context.Context, region string) (Catalog, error)
	GetInstance(ctx context.Context, region, name string) (Instance, bool, error)
	CreateInstance(ctx context.Context, input CreateInstanceInput) (Operation, error)
	GetOperation(ctx context.Context, region, id string) (Operation, error)
	ConfigureSSHFirewall(ctx context.Context, region, instanceName, sourceCIDR string) (Operation, error)
	GetStaticIP(ctx context.Context, region, name string) (StaticIP, bool, error)
	AllocateStaticIP(ctx context.Context, region, name string) (Operation, error)
	AttachStaticIP(ctx context.Context, region, staticIPName, instanceName string) (Operation, error)
	DetachStaticIP(ctx context.Context, region, staticIPName string) (Operation, error)
	ReleaseStaticIP(ctx context.Context, region, staticIPName string) (Operation, error)
	AccessDetails(ctx context.Context, region, instanceName string) (AccessDetails, error)
	StopInstance(ctx context.Context, region, instanceName string) (Operation, error)
	DeleteInstance(ctx context.Context, region, instanceName string) (Operation, error)
}
