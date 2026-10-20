package armclient

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	cache "github.com/patrickmn/go-cache"
	"k8s.io/apimachinery/pkg/labels"

	commonAzidentity "github.com/webdevops/go-common/azuresdk/azidentity"
	"github.com/webdevops/go-common/azuresdk/cloudconfig"
	"github.com/webdevops/go-common/azuresdk/prometheus/tracing"
	"github.com/webdevops/go-common/utils/to"
)

const (
	EnvVarServiceDiscoveryTtl                     = "AZURE_SERVICEDISCOVERY_CACHE_TTL"
	EnvVarServiceDiscoverySubscriptionId          = "AZURE_SERVICEDISCOVERY_SUBSCRIPTION_ID"
	EnvVarServiceDiscoverySubscriptionInclude     = "AZURE_SERVICEDISCOVERY_SUBSCRIPTION_INCLUDE"
	EnvVarServiceDiscoveryubscriptionExclude      = "AZURE_SERVICEDISCOVERY_SUBSCRIPTION_EXCLUDE"
	EnvVarServiceDiscoverySubscriptionTagSelector = "AZURE_SERVICEDISCOVERY_SUBSCRIPTION_TAG_SELECTOR"
)

type (
	ArmClient struct {
		TagManager *ArmClientTagManager

		cloud cloudconfig.CloudEnvironment

		logger *slog.Logger

		cache    *cache.Cache
		cacheTtl time.Duration

		serviceDiscovery struct {
			include struct {
				subscriptionIds []string
			}

			exclude struct {
				subscriptionIds []string
			}

			subscriptionTagSelector *labels.Selector
		}

		cred *azcore.TokenCredential

		userAgent string
	}
)

// NewArmClientFromEnvironment creates new Azure SDK ARM client from environment settings
func NewArmClientFromEnvironment(logger *slog.Logger, opts ...ClientOptionFunc) (*ArmClient, error) {
	var azureEnvironment string

	if azureEnvironment = os.Getenv("AZURE_ENVIRONMENT"); azureEnvironment == "" {
		logger.Info(`env var AZURE_ENVIRONMENT is not set, assuming "AzurePublicCloud"`)
		azureEnvironment = string(cloudconfig.AzurePublicCloud)

		if err := os.Setenv("AZURE_ENVIRONMENT", azureEnvironment); err != nil {
			return nil, fmt.Errorf(`unable to set AZURE_ENVIRONMENT`)
		}
	}

	return NewArmClientWithCloudName(azureEnvironment, logger, opts...)
}

// NewArmClient creates new Azure SDK ARM client
func NewArmClient(cloudConfig cloudconfig.CloudEnvironment, logger *slog.Logger, opts ...ClientOptionFunc) *ArmClient {
	client := &ArmClient{}
	client.cloud = cloudConfig

	client.logger = logger
	client.userAgent = "go-common/unknown"

	client.TagManager = &ArmClientTagManager{
		client: client,
		logger: logger.With(slog.String("component", "armClientTagManager")),
	}

	client.initCache()
	client.initServiceDiscovery()

	for _, opt := range opts {
		opt(client)
	}

	return client
}

// NewArmClientWithCloudName creates new Azure SDK ARM client with environment name as string
func NewArmClientWithCloudName(cloudName string, logger *slog.Logger, opts ...ClientOptionFunc) (*ArmClient, error) {
	cloudConfig, err := cloudconfig.NewCloudConfig(cloudName)
	if err != nil {
		return nil, err
	}

	return NewArmClient(cloudConfig, logger, opts...), nil
}

// init cache
func (azureClient *ArmClient) initCache() {
	cacheTtl := 60 * time.Minute
	if val := os.Getenv(EnvVarServiceDiscoveryTtl); val != "" {
		if ttl, err := time.ParseDuration(val); err == nil {
			cacheTtl = ttl
		} else {
			panic(fmt.Errorf(`%s is not a valid value, got "%v", expected duration`, EnvVarServiceDiscoveryTtl, val))
		}
	}
	azureClient.SetCacheTtl(cacheTtl)
}

// init serviceDiscovery settings
func (azureClient *ArmClient) initServiceDiscovery() {
	azureClient.serviceDiscovery.include.subscriptionIds = []string{}
	azureClient.serviceDiscovery.exclude.subscriptionIds = []string{}

	addStringListToList := func(list *[]string, val string) {
		// replace spaces and semicolons with commas,
		// we should be able to use both for easier usage in yaml files
		val = strings.ReplaceAll(val, " ", ",")
		val = strings.ReplaceAll(val, ";", ",")
		for _, row := range strings.Split(val, ",") {
			row = strings.TrimSpace(row)
			if row != "" {
				*list = append(*list, row)
			}
		}

		return
	}

	// use fixed list of subscription ids
	if val := os.Getenv(EnvVarServiceDiscoverySubscriptionId); val != "" {
		addStringListToList(&azureClient.serviceDiscovery.include.subscriptionIds, val)
	}

	if val := os.Getenv(EnvVarServiceDiscoverySubscriptionInclude); val != "" {
		addStringListToList(&azureClient.serviceDiscovery.include.subscriptionIds, val)
	}

	if val := os.Getenv(EnvVarServiceDiscoveryubscriptionExclude); val != "" {
		addStringListToList(&azureClient.serviceDiscovery.exclude.subscriptionIds, val)
	}

	// parse subscription tag selector (using kubernetes label selector)
	if val := os.Getenv(EnvVarServiceDiscoverySubscriptionTagSelector); val != "" {
		selector, err := labels.Parse(val)
		if err != nil {
			panic(err)
		}
		azureClient.serviceDiscovery.subscriptionTagSelector = &selector
	}
}

// LazyConnect triggers and logs connect message
func (azureClient *ArmClient) LazyConnect() error {
	ctx := context.Background()

	azureClient.logger.Info(
		`connecting to Azure`,
		slog.String("azureEnvironment", string(azureClient.cloud.Name)),
		slog.String("msGraph", azureClient.cloud.ActiveDirectoryAuthorityHost),
		slog.String("resourceManager", azureClient.cloud.Services[cloud.ResourceManager].Endpoint),
	)

	// try to get token
	scope := strings.TrimSuffix(azureClient.cloud.Services[cloud.ResourceManager].Endpoint, "/.default") + "/.default"
	accessToken, err := azureClient.GetCred().GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{scope}})
	if err != nil {
		return err
	}

	if tokenInfo := commonAzidentity.ParseAccessToken(accessToken); tokenInfo != nil {
		azureClient.logger.With(slog.Any("client", tokenInfo.ToMap())).Info(`using Azure client`, slog.String("token", tokenInfo.ToString()))
	} else {
		azureClient.logger.Warn(`unable to get Azure client information, cannot parse accesstoken`)
	}

	return nil
}

// Connect triggers and logs connect message
func (azureClient *ArmClient) Connect() error {
	ctx := context.Background()

	err := azureClient.LazyConnect()
	if err != nil {
		return err
	}

	subscriptionList, err := azureClient.ListSubscriptions(ctx)
	if err != nil {
		return err
	}

	azureClient.logger.Info(fmt.Sprintf(`found %v Azure Subscriptions`, len(subscriptionList)))
	for subscriptionId, subscription := range subscriptionList {
		azureClient.logger.Debug(fmt.Sprintf(`found Azure Subscription "%v" (%v)`, subscriptionId, to.String(subscription.DisplayName)))
	}

	return nil
}

// GetCred returns Azure ARM credential
func (azureClient *ArmClient) GetCred() azcore.TokenCredential {
	if azureClient.cred == nil {
		cred, err := commonAzidentity.NewAzDefaultCredential(azureClient.NewAzCoreClientOptions())
		if err != nil {
			panic(err)
		}
		azureClient.cred = &cred
	}

	return *azureClient.cred
}

// GetCloudName returns selected Azure Environment name (eg AzurePublicCloud)
func (azureClient *ArmClient) GetCloudName() cloudconfig.CloudName {
	return azureClient.cloud.Name
}

// GetCloudConfig returns selected Azure cloud/environment configuration
func (azureClient *ArmClient) GetCloudConfig() cloud.Configuration {
	return azureClient.cloud.Configuration
}

// NewAzCoreClientOptions returns new client options for all arm clients
func (azureClient *ArmClient) NewAzCoreClientOptions() *azcore.ClientOptions {
	clientOptions := azcore.ClientOptions{
		Cloud:            azureClient.cloud.Configuration,
		PerCallPolicies:  []policy.Policy{},
		PerRetryPolicies: azureClient.perRetryPolicies(),
		Telemetry:        azureClient.telemetryOptions(),
	}

	return &clientOptions
}

// NewArmClientOptions returns new client options for all arm clients
func (azureClient *ArmClient) NewArmClientOptions() *arm.ClientOptions {
	clientOptions := arm.ClientOptions{
		ClientOptions: policy.ClientOptions{
			Cloud:            azureClient.cloud.Configuration,
			Telemetry:        azureClient.telemetryOptions(),
			PerRetryPolicies: azureClient.perRetryPolicies(),
		},
	}

	return &clientOptions
}

// perRetryPolicies generates all default retry policies
func (azureClient *ArmClient) perRetryPolicies() (policies []policy.Policy) {
	// azure prometheus tracing
	if tracing.TracingIsEnabled() {
		policies = append(
			policies,
			tracing.NewTracingPolicy(),
		)
	}

	return
}

// telemetryOptions generates telemetry options
func (azureClient *ArmClient) telemetryOptions() policy.TelemetryOptions {
	return policy.TelemetryOptions{
		ApplicationID: strings.TrimSpace(azureClient.userAgent),
		Disabled:      false,
	}
}

// UseAzCliAuth use (force) az cli authentication
func (azureClient *ArmClient) UseAzCliAuth() {
	cred, err := commonAzidentity.NewAzCliCredential()
	if err != nil {
		panic(err)
	}
	azureClient.cred = &cred
}

// SetUserAgent set user agent for all API calls
func (azureClient *ArmClient) SetUserAgent(useragent string) {
	azureClient.userAgent = useragent
}

// SetCacheTtl set TTL for service discovery cache
func (azureClient *ArmClient) SetCacheTtl(ttl time.Duration) {
	azureClient.cacheTtl = ttl
	azureClient.cache = cache.New(ttl, 60*time.Second)
}

// SetSubscriptionFilter set subscription filter, other subscriptions will be ignored
//
// Deprecated: use SetSubscriptionID instead
func (azureClient *ArmClient) SetSubscriptionFilter(subscriptionId ...string) {
	azureClient.SetSubscriptionID(subscriptionId...)
}

// SetSubscriptionID set subscription filter, other subscriptions will be ignored
func (azureClient *ArmClient) SetSubscriptionID(subscriptionId ...string) {
	azureClient.serviceDiscovery.include.subscriptionIds = subscriptionId
}

// AddSubscriptionID add subscription filter, other subscriptions will be ignored
func (azureClient *ArmClient) AddSubscriptionID(subscriptionId ...string) {
	azureClient.IncludeSubscriptionID(subscriptionId...)
}

// IncludeSubscriptionID includes subscription in serviceDiscovery, other subscriptions will be ignored
func (azureClient *ArmClient) IncludeSubscriptionID(subscriptionId ...string) {
	azureClient.serviceDiscovery.include.subscriptionIds = append(
		azureClient.serviceDiscovery.include.subscriptionIds,
		subscriptionId...,
	)
}

// ExcludeSubscriptionID excludes subscription from serviceDiscovery, other subscriptions will be ignored
func (azureClient *ArmClient) ExcludeSubscriptionID(subscriptionId ...string) {
	azureClient.serviceDiscovery.exclude.subscriptionIds = append(
		azureClient.serviceDiscovery.exclude.subscriptionIds,
		subscriptionId...,
	)
}

func (azureClient *ArmClient) cacheData(identifier string, callback func() (interface{}, error)) (interface{}, error) {
	if v, ok := azureClient.cache.Get(identifier); ok {
		return v, nil
	}

	result, err := callback()
	if err == nil {
		azureClient.cache.SetDefault(identifier, result)
	}

	return result, err
}
