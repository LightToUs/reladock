package lightsail

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	awslightsail "github.com/aws/aws-sdk-go-v2/service/lightsail"
	awstypes "github.com/aws/aws-sdk-go-v2/service/lightsail/types"
)

type fakeLightsailAPI struct {
	lightsailAPI
	getRegionsFn               func(context.Context, *awslightsail.GetRegionsInput, ...func(*awslightsail.Options)) (*awslightsail.GetRegionsOutput, error)
	getBlueprintsFn            func(context.Context, *awslightsail.GetBlueprintsInput, ...func(*awslightsail.Options)) (*awslightsail.GetBlueprintsOutput, error)
	getBundlesFn               func(context.Context, *awslightsail.GetBundlesInput, ...func(*awslightsail.Options)) (*awslightsail.GetBundlesOutput, error)
	createInstancesFn          func(context.Context, *awslightsail.CreateInstancesInput, ...func(*awslightsail.Options)) (*awslightsail.CreateInstancesOutput, error)
	putInstancePublicPortsFn   func(context.Context, *awslightsail.PutInstancePublicPortsInput, ...func(*awslightsail.Options)) (*awslightsail.PutInstancePublicPortsOutput, error)
	getInstanceAccessDetailsFn func(context.Context, *awslightsail.GetInstanceAccessDetailsInput, ...func(*awslightsail.Options)) (*awslightsail.GetInstanceAccessDetailsOutput, error)
	getStaticIPFn              func(context.Context, *awslightsail.GetStaticIpInput, ...func(*awslightsail.Options)) (*awslightsail.GetStaticIpOutput, error)
	allocateStaticIPFn         func(context.Context, *awslightsail.AllocateStaticIpInput, ...func(*awslightsail.Options)) (*awslightsail.AllocateStaticIpOutput, error)
}

func (f *fakeLightsailAPI) GetRegions(ctx context.Context, in *awslightsail.GetRegionsInput, options ...func(*awslightsail.Options)) (*awslightsail.GetRegionsOutput, error) {
	return f.getRegionsFn(ctx, in, options...)
}
func (f *fakeLightsailAPI) GetBlueprints(ctx context.Context, in *awslightsail.GetBlueprintsInput, options ...func(*awslightsail.Options)) (*awslightsail.GetBlueprintsOutput, error) {
	return f.getBlueprintsFn(ctx, in, options...)
}
func (f *fakeLightsailAPI) GetBundles(ctx context.Context, in *awslightsail.GetBundlesInput, options ...func(*awslightsail.Options)) (*awslightsail.GetBundlesOutput, error) {
	return f.getBundlesFn(ctx, in, options...)
}
func (f *fakeLightsailAPI) CreateInstances(ctx context.Context, in *awslightsail.CreateInstancesInput, options ...func(*awslightsail.Options)) (*awslightsail.CreateInstancesOutput, error) {
	return f.createInstancesFn(ctx, in, options...)
}
func (f *fakeLightsailAPI) PutInstancePublicPorts(ctx context.Context, in *awslightsail.PutInstancePublicPortsInput, options ...func(*awslightsail.Options)) (*awslightsail.PutInstancePublicPortsOutput, error) {
	return f.putInstancePublicPortsFn(ctx, in, options...)
}
func (f *fakeLightsailAPI) GetInstanceAccessDetails(ctx context.Context, in *awslightsail.GetInstanceAccessDetailsInput, options ...func(*awslightsail.Options)) (*awslightsail.GetInstanceAccessDetailsOutput, error) {
	return f.getInstanceAccessDetailsFn(ctx, in, options...)
}
func (f *fakeLightsailAPI) GetStaticIp(ctx context.Context, in *awslightsail.GetStaticIpInput, options ...func(*awslightsail.Options)) (*awslightsail.GetStaticIpOutput, error) {
	return f.getStaticIPFn(ctx, in, options...)
}
func (f *fakeLightsailAPI) AllocateStaticIp(ctx context.Context, in *awslightsail.AllocateStaticIpInput, options ...func(*awslightsail.Options)) (*awslightsail.AllocateStaticIpOutput, error) {
	return f.allocateStaticIPFn(ctx, in, options...)
}

func TestAWSProviderRejectsDisallowedRegionBeforeClientConstruction(t *testing.T) {
	constructed := false
	provider := newAWSProviderWithFactory(Config{Enabled: true, AllowedRegions: []string{"us-east-1"}}, func(context.Context, string) (lightsailAPI, error) {
		constructed = true
		return nil, nil
	})

	_, err := provider.Catalog(context.Background(), "eu-west-1")
	if err == nil || !strings.Contains(err.Error(), "region is not allowed") {
		t.Fatalf("disallowed region error = %v", err)
	}
	if constructed {
		t.Fatal("constructed an SDK client for a disallowed region")
	}
}

func TestAWSProviderCatalogFiltersActiveLinuxResources(t *testing.T) {
	active := true
	inactive := false
	fake := &fakeLightsailAPI{}
	fake.getRegionsFn = func(_ context.Context, in *awslightsail.GetRegionsInput, _ ...func(*awslightsail.Options)) (*awslightsail.GetRegionsOutput, error) {
		if in.IncludeAvailabilityZones == nil || !*in.IncludeAvailabilityZones {
			t.Fatal("availability zones were not requested")
		}
		return &awslightsail.GetRegionsOutput{Regions: []awstypes.Region{
			{Name: awstypes.RegionNameUsEast1, AvailabilityZones: []awstypes.AvailabilityZone{{ZoneName: stringPtr("us-east-1a"), State: stringPtr("available")}}},
			{Name: awstypes.RegionNameEuWest1, AvailabilityZones: []awstypes.AvailabilityZone{{ZoneName: stringPtr("eu-west-1a"), State: stringPtr("available")}}},
		}}, nil
	}
	fake.getBlueprintsFn = func(_ context.Context, in *awslightsail.GetBlueprintsInput, _ ...func(*awslightsail.Options)) (*awslightsail.GetBlueprintsOutput, error) {
		if in.IncludeInactive == nil || *in.IncludeInactive {
			t.Fatal("inactive blueprints requested")
		}
		return &awslightsail.GetBlueprintsOutput{Blueprints: []awstypes.Blueprint{
			{BlueprintId: stringPtr("linux"), Name: stringPtr("Linux"), Platform: awstypes.InstancePlatformLinuxUnix, IsActive: &active},
			{BlueprintId: stringPtr("windows"), Name: stringPtr("Windows"), Platform: awstypes.InstancePlatformWindows, IsActive: &active},
			{BlueprintId: stringPtr("old"), Name: stringPtr("Old"), Platform: awstypes.InstancePlatformLinuxUnix, IsActive: &inactive},
		}}, nil
	}
	fake.getBundlesFn = func(_ context.Context, in *awslightsail.GetBundlesInput, _ ...func(*awslightsail.Options)) (*awslightsail.GetBundlesOutput, error) {
		return &awslightsail.GetBundlesOutput{Bundles: []awstypes.Bundle{
			{BundleId: stringPtr("linux-small"), Name: stringPtr("Small"), IsActive: &active, SupportedPlatforms: []awstypes.InstancePlatform{awstypes.InstancePlatformLinuxUnix}},
			{BundleId: stringPtr("windows-small"), Name: stringPtr("Windows"), IsActive: &active, SupportedPlatforms: []awstypes.InstancePlatform{awstypes.InstancePlatformWindows}},
		}}, nil
	}
	provider := newAWSProviderWithFactory(Config{Enabled: true, AllowedRegions: []string{"us-east-1"}, AWSOperationTimeout: time.Second}, func(context.Context, string) (lightsailAPI, error) { return fake, nil })

	catalog, err := provider.Catalog(context.Background(), "us-east-1")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if !reflect.DeepEqual(catalog.AvailabilityZones, []AvailabilityZone{{Name: "us-east-1a", State: "available"}}) || len(catalog.Blueprints) != 1 || catalog.Blueprints[0].ID != "linux" || len(catalog.Bundles) != 1 || catalog.Bundles[0].ID != "linux-small" {
		t.Fatalf("unfiltered catalog: %+v", catalog)
	}
}

func TestAWSProviderMapsCreateTagsOperationAndFirewall(t *testing.T) {
	fake := &fakeLightsailAPI{}
	fake.createInstancesFn = func(_ context.Context, in *awslightsail.CreateInstancesInput, _ ...func(*awslightsail.Options)) (*awslightsail.CreateInstancesOutput, error) {
		if !reflect.DeepEqual(in.InstanceNames, []string{"reladock-ls-123"}) || stringValue(in.AvailabilityZone) != "us-east-1a" || stringValue(in.BlueprintId) != "debian" || stringValue(in.BundleId) != "small" {
			t.Fatalf("create input: %+v", in)
		}
		gotTags := make([]string, 0, len(in.Tags))
		for _, tag := range in.Tags {
			gotTags = append(gotTags, stringValue(tag.Key)+"="+stringValue(tag.Value))
		}
		if !reflect.DeepEqual(gotTags, []string{"a=first", "z=last"}) {
			t.Fatalf("nondeterministic tags: %v", gotTags)
		}
		return &awslightsail.CreateInstancesOutput{Operations: []awstypes.Operation{{Id: stringPtr("op-create"), Status: awstypes.OperationStatusStarted}}}, nil
	}
	fake.putInstancePublicPortsFn = func(_ context.Context, in *awslightsail.PutInstancePublicPortsInput, _ ...func(*awslightsail.Options)) (*awslightsail.PutInstancePublicPortsOutput, error) {
		want := []awstypes.PortInfo{{FromPort: 22, ToPort: 22, Protocol: awstypes.NetworkProtocolTcp, Cidrs: []string{"192.0.2.8/32"}}}
		if stringValue(in.InstanceName) != "reladock-ls-123" || !reflect.DeepEqual(in.PortInfos, want) {
			t.Fatalf("firewall input: %+v", in)
		}
		return &awslightsail.PutInstancePublicPortsOutput{Operation: &awstypes.Operation{Id: stringPtr("op-fw"), Status: awstypes.OperationStatusSucceeded}}, nil
	}
	provider := newAWSProviderWithFactory(Config{Enabled: true, AllowedRegions: []string{"us-east-1"}, AWSOperationTimeout: time.Second}, func(context.Context, string) (lightsailAPI, error) { return fake, nil })

	op, err := provider.CreateInstance(context.Background(), CreateInstanceInput{Region: "us-east-1", AvailabilityZone: "us-east-1a", BlueprintID: "debian", BundleID: "small", Name: "reladock-ls-123", Tags: map[string]string{"z": "last", "a": "first"}})
	if err != nil || op.ID != "op-create" || op.Status != "started" {
		t.Fatalf("create result: %+v %v", op, err)
	}
	op, err = provider.ConfigureSSHFirewall(context.Background(), "us-east-1", "reladock-ls-123", "192.0.2.8/32")
	if err != nil || op.ID != "op-fw" || op.Status != "succeeded" {
		t.Fatalf("firewall result: %+v %v", op, err)
	}
}

func TestAWSProviderMapsTemporaryAccessAndStaticIP(t *testing.T) {
	expires := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	fake := &fakeLightsailAPI{}
	fake.getInstanceAccessDetailsFn = func(_ context.Context, in *awslightsail.GetInstanceAccessDetailsInput, _ ...func(*awslightsail.Options)) (*awslightsail.GetInstanceAccessDetailsOutput, error) {
		if in.Protocol != awstypes.InstanceAccessProtocolSsh || stringValue(in.InstanceName) != "node" {
			t.Fatalf("access input: %+v", in)
		}
		return &awslightsail.GetInstanceAccessDetailsOutput{AccessDetails: &awstypes.InstanceAccessDetails{
			IpAddress: stringPtr("203.0.113.8"), Username: stringPtr("admin"), PrivateKey: stringPtr("private"), CertKey: stringPtr("cert"), ExpiresAt: &expires,
			HostKeys: []awstypes.HostKeyAttributes{{Algorithm: stringPtr("ssh-ed25519"), PublicKey: stringPtr("ssh-ed25519 AAAA")}},
		}}, nil
	}
	fake.getStaticIPFn = func(_ context.Context, _ *awslightsail.GetStaticIpInput, _ ...func(*awslightsail.Options)) (*awslightsail.GetStaticIpOutput, error) {
		return &awslightsail.GetStaticIpOutput{StaticIp: &awstypes.StaticIp{Name: stringPtr("ip-node"), IpAddress: stringPtr("203.0.113.8"), AttachedTo: stringPtr("node"), IsAttached: boolPtr(true)}}, nil
	}
	fake.allocateStaticIPFn = func(_ context.Context, in *awslightsail.AllocateStaticIpInput, _ ...func(*awslightsail.Options)) (*awslightsail.AllocateStaticIpOutput, error) {
		if stringValue(in.StaticIpName) != "ip-node" {
			t.Fatalf("static IP name: %q", stringValue(in.StaticIpName))
		}
		return &awslightsail.AllocateStaticIpOutput{Operations: []awstypes.Operation{{Id: stringPtr("op-allocate"), Status: awstypes.OperationStatusSucceeded}}}, nil
	}
	provider := newAWSProviderWithFactory(Config{Enabled: true, AllowedRegions: []string{"us-east-1"}, AWSOperationTimeout: time.Second}, func(context.Context, string) (lightsailAPI, error) { return fake, nil })

	access, err := provider.AccessDetails(context.Background(), "us-east-1", "node")
	if err != nil || access.IPAddress != "203.0.113.8" || access.Username != "admin" || access.PrivateKey != "private" || access.Certificate != "cert" || !access.ExpiresAt.Equal(expires) || !reflect.DeepEqual(access.HostKeys, []HostKey{{Algorithm: "ssh-ed25519", PublicKey: "ssh-ed25519 AAAA"}}) {
		t.Fatalf("access mapping: %+v %v", access, err)
	}
	staticIP, found, err := provider.GetStaticIP(context.Background(), "us-east-1", "ip-node")
	if err != nil || !found || staticIP.AttachedTo != "node" || staticIP.IPAddress != "203.0.113.8" {
		t.Fatalf("static IP mapping: %+v %t %v", staticIP, found, err)
	}
	op, err := provider.AllocateStaticIP(context.Background(), "us-east-1", "ip-node")
	if err != nil || op.ID != "op-allocate" {
		t.Fatalf("allocate mapping: %+v %v", op, err)
	}
}

func TestAWSProviderSanitizesErrorsAndAppliesDeadline(t *testing.T) {
	fake := &fakeLightsailAPI{}
	fake.getRegionsFn = func(ctx context.Context, _ *awslightsail.GetRegionsInput, _ ...func(*awslightsail.Options)) (*awslightsail.GetRegionsOutput, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 200*time.Millisecond {
			t.Fatal("provider call has no bounded deadline")
		}
		return nil, errors.New("Authorization: Bearer live-secret-value")
	}
	provider := newAWSProviderWithFactory(Config{Enabled: true, AllowedRegions: []string{"us-east-1"}, AWSOperationTimeout: 100 * time.Millisecond}, func(context.Context, string) (lightsailAPI, error) { return fake, nil })

	_, err := provider.Catalog(context.Background(), "us-east-1")
	if err == nil || strings.Contains(err.Error(), "live-secret-value") || err.Error() != "Lightsail provider request failed" {
		t.Fatalf("unsanitized provider error: %v", err)
	}
}

func stringPtr(value string) *string { return &value }
func boolPtr(value bool) *bool       { return &value }
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
