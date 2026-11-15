package lightsail

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awslightsail "github.com/aws/aws-sdk-go-v2/service/lightsail"
	awstypes "github.com/aws/aws-sdk-go-v2/service/lightsail/types"
)

const sanitizedProviderError = "Lightsail provider request failed"

type lightsailAPI interface {
	GetRegions(context.Context, *awslightsail.GetRegionsInput, ...func(*awslightsail.Options)) (*awslightsail.GetRegionsOutput, error)
	GetBlueprints(context.Context, *awslightsail.GetBlueprintsInput, ...func(*awslightsail.Options)) (*awslightsail.GetBlueprintsOutput, error)
	GetBundles(context.Context, *awslightsail.GetBundlesInput, ...func(*awslightsail.Options)) (*awslightsail.GetBundlesOutput, error)
	GetInstance(context.Context, *awslightsail.GetInstanceInput, ...func(*awslightsail.Options)) (*awslightsail.GetInstanceOutput, error)
	CreateInstances(context.Context, *awslightsail.CreateInstancesInput, ...func(*awslightsail.Options)) (*awslightsail.CreateInstancesOutput, error)
	GetOperation(context.Context, *awslightsail.GetOperationInput, ...func(*awslightsail.Options)) (*awslightsail.GetOperationOutput, error)
	PutInstancePublicPorts(context.Context, *awslightsail.PutInstancePublicPortsInput, ...func(*awslightsail.Options)) (*awslightsail.PutInstancePublicPortsOutput, error)
	GetStaticIp(context.Context, *awslightsail.GetStaticIpInput, ...func(*awslightsail.Options)) (*awslightsail.GetStaticIpOutput, error)
	AllocateStaticIp(context.Context, *awslightsail.AllocateStaticIpInput, ...func(*awslightsail.Options)) (*awslightsail.AllocateStaticIpOutput, error)
	AttachStaticIp(context.Context, *awslightsail.AttachStaticIpInput, ...func(*awslightsail.Options)) (*awslightsail.AttachStaticIpOutput, error)
	DetachStaticIp(context.Context, *awslightsail.DetachStaticIpInput, ...func(*awslightsail.Options)) (*awslightsail.DetachStaticIpOutput, error)
	ReleaseStaticIp(context.Context, *awslightsail.ReleaseStaticIpInput, ...func(*awslightsail.Options)) (*awslightsail.ReleaseStaticIpOutput, error)
	GetInstanceAccessDetails(context.Context, *awslightsail.GetInstanceAccessDetailsInput, ...func(*awslightsail.Options)) (*awslightsail.GetInstanceAccessDetailsOutput, error)
	StopInstance(context.Context, *awslightsail.StopInstanceInput, ...func(*awslightsail.Options)) (*awslightsail.StopInstanceOutput, error)
	DeleteInstance(context.Context, *awslightsail.DeleteInstanceInput, ...func(*awslightsail.Options)) (*awslightsail.DeleteInstanceOutput, error)
}

type clientFactory func(context.Context, string) (lightsailAPI, error)

type awsProvider struct {
	cfg     Config
	factory clientFactory
	mu      sync.Mutex
	clients map[string]lightsailAPI
}

func NewAWSProvider(ctx context.Context, cfg Config) (Provider, error) {
	if !cfg.Enabled {
		return nil, errors.New("Lightsail connector is disabled")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 30 * time.Second
	httpClient := &http.Client{Transport: transport, Timeout: cfg.AWSOperationTimeout}
	shared, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithHTTPClient(httpClient))
	if err != nil {
		return nil, errors.New(sanitizedProviderError)
	}
	return newAWSProviderWithFactory(cfg, func(_ context.Context, region string) (lightsailAPI, error) {
		regional := shared.Copy()
		regional.Region = region
		return awslightsail.NewFromConfig(regional), nil
	}), nil
}

func newAWSProviderWithFactory(cfg Config, factory clientFactory) *awsProvider {
	return &awsProvider{cfg: cfg, factory: factory, clients: make(map[string]lightsailAPI)}
}

func (p *awsProvider) client(ctx context.Context, region string) (lightsailAPI, error) {
	if !p.cfg.AllowsRegion(region) {
		return nil, errors.New("Lightsail region is not allowed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if client := p.clients[region]; client != nil {
		return client, nil
	}
	client, err := p.factory(ctx, region)
	if err != nil || client == nil {
		return nil, errors.New(sanitizedProviderError)
	}
	p.clients[region] = client
	return client, nil
}

func (p *awsProvider) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := p.cfg.AWSOperationTimeout
	if timeout <= 0 {
		timeout = defaultAWSOperationTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (p *awsProvider) Catalog(ctx context.Context, region string) (Catalog, error) {
	client, err := p.client(ctx, region)
	if err != nil {
		return Catalog{}, err
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	includeZones := true
	regions, err := client.GetRegions(callCtx, &awslightsail.GetRegionsInput{IncludeAvailabilityZones: &includeZones})
	if err != nil {
		return Catalog{}, providerError(callCtx, err)
	}
	catalog := Catalog{Region: region}
	for _, item := range regions.Regions {
		if string(item.Name) != region {
			continue
		}
		for _, zone := range item.AvailabilityZones {
			catalog.AvailabilityZones = append(catalog.AvailabilityZones, AvailabilityZone{Name: aws.ToString(zone.ZoneName), State: aws.ToString(zone.State)})
		}
	}
	includeInactive := false
	var blueprintToken *string
	for {
		out, callErr := client.GetBlueprints(callCtx, &awslightsail.GetBlueprintsInput{IncludeInactive: &includeInactive, PageToken: blueprintToken})
		if callErr != nil {
			return Catalog{}, providerError(callCtx, callErr)
		}
		for _, item := range out.Blueprints {
			if !aws.ToBool(item.IsActive) || item.Platform != awstypes.InstancePlatformLinuxUnix || aws.ToString(item.BlueprintId) == "" {
				continue
			}
			catalog.Blueprints = append(catalog.Blueprints, Blueprint{ID: aws.ToString(item.BlueprintId), Name: aws.ToString(item.Name), Platform: string(item.Platform), Active: true})
		}
		blueprintToken = out.NextPageToken
		if blueprintToken == nil || *blueprintToken == "" {
			break
		}
	}
	var bundleToken *string
	for {
		out, callErr := client.GetBundles(callCtx, &awslightsail.GetBundlesInput{IncludeInactive: &includeInactive, PageToken: bundleToken})
		if callErr != nil {
			return Catalog{}, providerError(callCtx, callErr)
		}
		for _, item := range out.Bundles {
			if !aws.ToBool(item.IsActive) || !supportsLinux(item.SupportedPlatforms) || aws.ToString(item.BundleId) == "" {
				continue
			}
			platforms := make([]string, 0, len(item.SupportedPlatforms))
			for _, platform := range item.SupportedPlatforms {
				platforms = append(platforms, string(platform))
			}
			catalog.Bundles = append(catalog.Bundles, Bundle{ID: aws.ToString(item.BundleId), Name: aws.ToString(item.Name), PriceUSD: float64(aws.ToFloat32(item.Price)), CPUCount: aws.ToInt32(item.CpuCount), RAMGiB: aws.ToFloat32(item.RamSizeInGb), DiskGiB: aws.ToInt32(item.DiskSizeInGb), SupportedPlatforms: platforms, Active: true})
		}
		bundleToken = out.NextPageToken
		if bundleToken == nil || *bundleToken == "" {
			break
		}
	}
	return catalog, nil
}

func supportsLinux(platforms []awstypes.InstancePlatform) bool {
	for _, platform := range platforms {
		if platform == awstypes.InstancePlatformLinuxUnix {
			return true
		}
	}
	return false
}

func (p *awsProvider) GetInstance(ctx context.Context, region, name string) (Instance, bool, error) {
	client, err := p.client(ctx, region)
	if err != nil {
		return Instance{}, false, err
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	out, err := client.GetInstance(callCtx, &awslightsail.GetInstanceInput{InstanceName: aws.String(name)})
	if err != nil {
		if isNotFound(err) {
			return Instance{}, false, nil
		}
		return Instance{}, false, providerError(callCtx, err)
	}
	if out.Instance == nil {
		return Instance{}, false, errors.New(sanitizedProviderError)
	}
	return mapInstance(*out.Instance), true, nil
}

func mapInstance(item awstypes.Instance) Instance {
	result := Instance{Name: aws.ToString(item.Name), ARN: aws.ToString(item.Arn), BlueprintID: aws.ToString(item.BlueprintId), BundleID: aws.ToString(item.BundleId), PublicIP: aws.ToString(item.PublicIpAddress), Tags: make(map[string]string)}
	if item.Location != nil {
		result.Region = string(item.Location.RegionName)
		result.AvailabilityZone = aws.ToString(item.Location.AvailabilityZone)
	}
	if item.State != nil {
		result.State = aws.ToString(item.State.Name)
	}
	for _, tag := range item.Tags {
		result.Tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return result
}

func (p *awsProvider) CreateInstance(ctx context.Context, input CreateInstanceInput) (Operation, error) {
	client, err := p.client(ctx, input.Region)
	if err != nil {
		return Operation{}, err
	}
	keys := make([]string, 0, len(input.Tags))
	for key := range input.Tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	tags := make([]awstypes.Tag, 0, len(keys))
	for _, key := range keys {
		tags = append(tags, awstypes.Tag{Key: aws.String(key), Value: aws.String(input.Tags[key])})
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	out, err := client.CreateInstances(callCtx, &awslightsail.CreateInstancesInput{AvailabilityZone: aws.String(input.AvailabilityZone), BlueprintId: aws.String(input.BlueprintID), BundleId: aws.String(input.BundleID), InstanceNames: []string{input.Name}, Tags: tags})
	if err != nil {
		return Operation{}, providerError(callCtx, err)
	}
	return firstOperation(out.Operations)
}

func (p *awsProvider) GetOperation(ctx context.Context, region, id string) (Operation, error) {
	client, err := p.client(ctx, region)
	if err != nil {
		return Operation{}, err
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	out, err := client.GetOperation(callCtx, &awslightsail.GetOperationInput{OperationId: aws.String(id)})
	if err != nil || out.Operation == nil {
		return Operation{}, providerError(callCtx, err)
	}
	return mapOperation(*out.Operation), nil
}

func (p *awsProvider) ConfigureSSHFirewall(ctx context.Context, region, instanceName, sourceCIDR string) (Operation, error) {
	client, err := p.client(ctx, region)
	if err != nil {
		return Operation{}, err
	}
	port := awstypes.PortInfo{FromPort: 22, ToPort: 22, Protocol: awstypes.NetworkProtocolTcp}
	if strings.Contains(sourceCIDR, ":") {
		port.Ipv6Cidrs = []string{sourceCIDR}
	} else {
		port.Cidrs = []string{sourceCIDR}
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	out, err := client.PutInstancePublicPorts(callCtx, &awslightsail.PutInstancePublicPortsInput{InstanceName: aws.String(instanceName), PortInfos: []awstypes.PortInfo{port}})
	if err != nil || out.Operation == nil {
		return Operation{}, providerError(callCtx, err)
	}
	return mapOperation(*out.Operation), nil
}

func (p *awsProvider) GetStaticIP(ctx context.Context, region, name string) (StaticIP, bool, error) {
	client, err := p.client(ctx, region)
	if err != nil {
		return StaticIP{}, false, err
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	out, err := client.GetStaticIp(callCtx, &awslightsail.GetStaticIpInput{StaticIpName: aws.String(name)})
	if err != nil {
		if isNotFound(err) {
			return StaticIP{}, false, nil
		}
		return StaticIP{}, false, providerError(callCtx, err)
	}
	if out.StaticIp == nil {
		return StaticIP{}, false, errors.New(sanitizedProviderError)
	}
	item := out.StaticIp
	result := StaticIP{Name: aws.ToString(item.Name), IPAddress: aws.ToString(item.IpAddress), AttachedTo: aws.ToString(item.AttachedTo), IsAttached: aws.ToBool(item.IsAttached), SupportCode: aws.ToString(item.SupportCode), ResourceType: string(item.ResourceType)}
	if item.Location != nil {
		result.ResourceLocation = string(item.Location.RegionName)
	}
	return result, true, nil
}

func (p *awsProvider) AllocateStaticIP(ctx context.Context, region, name string) (Operation, error) {
	client, err := p.client(ctx, region)
	if err != nil {
		return Operation{}, err
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	out, err := client.AllocateStaticIp(callCtx, &awslightsail.AllocateStaticIpInput{StaticIpName: aws.String(name)})
	if err != nil {
		return Operation{}, providerError(callCtx, err)
	}
	return firstOperation(out.Operations)
}

func (p *awsProvider) AttachStaticIP(ctx context.Context, region, staticIPName, instanceName string) (Operation, error) {
	client, err := p.client(ctx, region)
	if err != nil {
		return Operation{}, err
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	out, err := client.AttachStaticIp(callCtx, &awslightsail.AttachStaticIpInput{StaticIpName: aws.String(staticIPName), InstanceName: aws.String(instanceName)})
	if err != nil {
		return Operation{}, providerError(callCtx, err)
	}
	return firstOperation(out.Operations)
}

func (p *awsProvider) DetachStaticIP(ctx context.Context, region, staticIPName string) (Operation, error) {
	client, err := p.client(ctx, region)
	if err != nil {
		return Operation{}, err
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	out, err := client.DetachStaticIp(callCtx, &awslightsail.DetachStaticIpInput{StaticIpName: aws.String(staticIPName)})
	if err != nil {
		return Operation{}, providerError(callCtx, err)
	}
	return firstOperation(out.Operations)
}

func (p *awsProvider) ReleaseStaticIP(ctx context.Context, region, staticIPName string) (Operation, error) {
	client, err := p.client(ctx, region)
	if err != nil {
		return Operation{}, err
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	out, err := client.ReleaseStaticIp(callCtx, &awslightsail.ReleaseStaticIpInput{StaticIpName: aws.String(staticIPName)})
	if err != nil {
		return Operation{}, providerError(callCtx, err)
	}
	return firstOperation(out.Operations)
}

func (p *awsProvider) AccessDetails(ctx context.Context, region, instanceName string) (AccessDetails, error) {
	client, err := p.client(ctx, region)
	if err != nil {
		return AccessDetails{}, err
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	out, err := client.GetInstanceAccessDetails(callCtx, &awslightsail.GetInstanceAccessDetailsInput{InstanceName: aws.String(instanceName), Protocol: awstypes.InstanceAccessProtocolSsh})
	if err != nil || out.AccessDetails == nil {
		return AccessDetails{}, providerError(callCtx, err)
	}
	item := out.AccessDetails
	result := AccessDetails{IPAddress: aws.ToString(item.IpAddress), Username: aws.ToString(item.Username), PrivateKey: aws.ToString(item.PrivateKey), Certificate: aws.ToString(item.CertKey)}
	if item.ExpiresAt != nil {
		result.ExpiresAt = *item.ExpiresAt
	}
	for _, key := range item.HostKeys {
		result.HostKeys = append(result.HostKeys, HostKey{Algorithm: aws.ToString(key.Algorithm), PublicKey: aws.ToString(key.PublicKey)})
	}
	return result, nil
}

func (p *awsProvider) StopInstance(ctx context.Context, region, instanceName string) (Operation, error) {
	client, err := p.client(ctx, region)
	if err != nil {
		return Operation{}, err
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	out, err := client.StopInstance(callCtx, &awslightsail.StopInstanceInput{InstanceName: aws.String(instanceName)})
	if err != nil {
		return Operation{}, providerError(callCtx, err)
	}
	return firstOperation(out.Operations)
}

func (p *awsProvider) DeleteInstance(ctx context.Context, region, instanceName string) (Operation, error) {
	client, err := p.client(ctx, region)
	if err != nil {
		return Operation{}, err
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	out, err := client.DeleteInstance(callCtx, &awslightsail.DeleteInstanceInput{InstanceName: aws.String(instanceName)})
	if err != nil {
		return Operation{}, providerError(callCtx, err)
	}
	return firstOperation(out.Operations)
}

func firstOperation(operations []awstypes.Operation) (Operation, error) {
	if len(operations) != 1 || aws.ToString(operations[0].Id) == "" {
		return Operation{}, errors.New(sanitizedProviderError)
	}
	return mapOperation(operations[0]), nil
}

func mapOperation(item awstypes.Operation) Operation {
	result := Operation{ID: aws.ToString(item.Id), Status: strings.ToLower(string(item.Status)), ErrorCode: aws.ToString(item.ErrorCode)}
	if item.CreatedAt != nil {
		result.CreatedAt = *item.CreatedAt
	}
	return result
}

func isNotFound(err error) bool {
	var notFound *awstypes.NotFoundException
	return errors.As(err, &notFound)
}

func providerError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return errors.New(sanitizedProviderError)
}
