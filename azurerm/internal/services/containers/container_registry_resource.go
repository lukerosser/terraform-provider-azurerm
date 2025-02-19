package containers

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/preview/containerregistry/mgmt/2020-11-01-preview/containerregistry"
	"github.com/hashicorp/go-azure-helpers/response"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/helper/validation"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/azure"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/tf"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/validate"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/clients"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/location"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/tags"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/tf/suppress"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/timeouts"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/utils"
)

func resourceContainerRegistry() *schema.Resource {
	return &schema.Resource{
		Create: resourceContainerRegistryCreate,
		Read:   resourceContainerRegistryRead,
		Update: resourceContainerRegistryUpdate,
		Delete: resourceContainerRegistryDelete,
		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},
		MigrateState:  ResourceContainerRegistryMigrateState,
		SchemaVersion: 2,

		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(30 * time.Minute),
			Read:   schema.DefaultTimeout(5 * time.Minute),
			Update: schema.DefaultTimeout(30 * time.Minute),
			Delete: schema.DefaultTimeout(30 * time.Minute),
		},

		Schema: map[string]*schema.Schema{
			"name": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: ValidateContainerRegistryName,
			},

			"resource_group_name": azure.SchemaResourceGroupName(),

			"location": azure.SchemaLocation(),

			"sku": {
				Type:             schema.TypeString,
				Optional:         true,
				Default:          string(containerregistry.Classic),
				DiffSuppressFunc: suppress.CaseDifference,
				ValidateFunc: validation.StringInSlice([]string{
					string(containerregistry.Classic),
					string(containerregistry.Basic),
					string(containerregistry.Standard),
					string(containerregistry.Premium),
				}, true),
			},

			"admin_enabled": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
			},

			"georeplication_locations": {
				Type:     schema.TypeSet,
				MinItems: 1,
				Optional: true,
				Elem: &schema.Schema{
					Type:         schema.TypeString,
					ValidateFunc: validation.StringIsNotEmpty,
				},
				Set: location.HashCode,
			},

			"public_network_access_enabled": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  true,
			},

			"storage_account_id": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
			},

			"login_server": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"admin_username": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"admin_password": {
				Type:      schema.TypeString,
				Computed:  true,
				Sensitive: true,
			},

			"network_rule_set": {
				Type:       schema.TypeList,
				Optional:   true,
				Computed:   true,
				MaxItems:   1,
				ConfigMode: schema.SchemaConfigModeAttr, // make sure we can set this to an empty array for Premium -> Basic
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"default_action": {
							Type:     schema.TypeString,
							Optional: true,
							Default:  containerregistry.DefaultActionAllow,
							ValidateFunc: validation.StringInSlice([]string{
								string(containerregistry.DefaultActionAllow),
								string(containerregistry.DefaultActionDeny),
							}, false),
						},

						"ip_rule": {
							Type:       schema.TypeSet,
							Optional:   true,
							ConfigMode: schema.SchemaConfigModeAttr,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"action": {
										Type:     schema.TypeString,
										Required: true,
										ValidateFunc: validation.StringInSlice([]string{
											string(containerregistry.Allow),
										}, false),
									},
									"ip_range": {
										Type:         schema.TypeString,
										Required:     true,
										ValidateFunc: validate.CIDR,
									},
								},
							},
						},

						"virtual_network": {
							Type:       schema.TypeSet,
							Optional:   true,
							ConfigMode: schema.SchemaConfigModeAttr,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"action": {
										Type:     schema.TypeString,
										Required: true,
										ValidateFunc: validation.StringInSlice([]string{
											string(containerregistry.Allow),
										}, false),
									},
									"subnet_id": {
										Type:         schema.TypeString,
										Required:     true,
										ValidateFunc: azure.ValidateResourceID,
									},
								},
							},
						},
					},
				},
			},

			"retention_policy": {
				Type:       schema.TypeList,
				MaxItems:   1,
				Optional:   true,
				Computed:   true,
				ConfigMode: schema.SchemaConfigModeAttr,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"days": {
							Type:     schema.TypeInt,
							Optional: true,
							Default:  7,
						},

						"enabled": {
							Type:     schema.TypeBool,
							Optional: true,
							Default:  false,
						},
					},
				},
			},

			"trust_policy": {
				Type:       schema.TypeList,
				MaxItems:   1,
				Optional:   true,
				Computed:   true,
				ConfigMode: schema.SchemaConfigModeAttr,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"enabled": {
							Type:     schema.TypeBool,
							Optional: true,
							Default:  false,
						},
					},
				},
			},

			"tags": tags.Schema(),
		},

		CustomizeDiff: func(d *schema.ResourceDiff, v interface{}) error {
			sku := d.Get("sku").(string)
			geoReplicationLocations := d.Get("georeplication_locations").(*schema.Set)
			// if locations have been specified for geo-replication then, the SKU has to be Premium
			if geoReplicationLocations != nil && geoReplicationLocations.Len() > 0 && !strings.EqualFold(sku, string(containerregistry.Premium)) {
				return fmt.Errorf("ACR geo-replication can only be applied when using the Premium Sku.")
			}

			retentionPolicyEnabled, ok := d.GetOk("retention_policy.0.enabled")
			if ok && retentionPolicyEnabled.(bool) && !strings.EqualFold(sku, string(containerregistry.Premium)) {
				return fmt.Errorf("ACR retention policy can only be applied when using the Premium Sku. If you are downgrading from a Premium SKU please set retention_policy {}")
			}

			trustPolicyEnabled, ok := d.GetOk("trust_policy.0.enabled")
			if ok && trustPolicyEnabled.(bool) && !strings.EqualFold(sku, string(containerregistry.Premium)) {
				return fmt.Errorf("ACR trust policy can only be applied when using the Premium Sku. If you are downgrading from a Premium SKU please set trust_policy {}")
			}

			return nil
		},
	}
}

func resourceContainerRegistryCreate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Containers.RegistriesClient
	ctx, cancel := timeouts.ForCreate(meta.(*clients.Client).StopContext, d)
	defer cancel()
	log.Printf("[INFO] preparing arguments for  Container Registry creation.")

	resourceGroup := d.Get("resource_group_name").(string)
	name := d.Get("name").(string)

	if d.IsNewResource() {
		existing, err := client.Get(ctx, resourceGroup, name)
		if err != nil {
			if !utils.ResponseWasNotFound(existing.Response) {
				return fmt.Errorf("Error checking for presence of existing Container Registry %q (Resource Group %q): %s", name, resourceGroup, err)
			}
		}

		if existing.ID != nil && *existing.ID != "" {
			return tf.ImportAsExistsError("azurerm_container_registry", *existing.ID)
		}
	}

	availabilityRequest := containerregistry.RegistryNameCheckRequest{
		Name: utils.String(name),
		Type: utils.String("Microsoft.ContainerRegistry/registries"),
	}
	available, err := client.CheckNameAvailability(ctx, availabilityRequest)
	if err != nil {
		return fmt.Errorf("Error checking if the name %q was available: %+v", name, err)
	}

	if !*available.NameAvailable {
		return fmt.Errorf("The name %q used for the Container Registry needs to be globally unique and isn't available: %s", name, *available.Message)
	}

	location := azure.NormalizeLocation(d.Get("location").(string))
	sku := d.Get("sku").(string)
	adminUserEnabled := d.Get("admin_enabled").(bool)
	t := d.Get("tags").(map[string]interface{})
	geoReplicationLocations := d.Get("georeplication_locations").(*schema.Set)

	networkRuleSet := expandNetworkRuleSet(d.Get("network_rule_set").([]interface{}))
	if networkRuleSet != nil && !strings.EqualFold(sku, string(containerregistry.Premium)) {
		return fmt.Errorf("`network_rule_set_set` can only be specified for a Premium Sku. If you are reverting from a Premium to Basic SKU plese set network_rule_set = []")
	}

	retentionPolicyRaw := d.Get("retention_policy").([]interface{})
	retentionPolicy := expandRetentionPolicy(retentionPolicyRaw)

	trustPolicyRaw := d.Get("trust_policy").([]interface{})
	trustPolicy := expandTrustPolicy(trustPolicyRaw)

	publicNetworkAccess := containerregistry.PublicNetworkAccessEnabled
	if !d.Get("public_network_access_enabled").(bool) {
		publicNetworkAccess = containerregistry.PublicNetworkAccessDisabled
	}
	parameters := containerregistry.Registry{
		Location: &location,
		Sku: &containerregistry.Sku{
			Name: containerregistry.SkuName(sku),
			Tier: containerregistry.SkuTier(sku),
		},
		RegistryProperties: &containerregistry.RegistryProperties{
			AdminUserEnabled: utils.Bool(adminUserEnabled),
			NetworkRuleSet:   networkRuleSet,
			Policies: &containerregistry.Policies{
				RetentionPolicy: retentionPolicy,
				TrustPolicy:     trustPolicy,
			},
			PublicNetworkAccess: publicNetworkAccess,
		},

		Tags: tags.Expand(t),
	}

	if v, ok := d.GetOk("storage_account_id"); ok {
		if !strings.EqualFold(sku, string(containerregistry.Classic)) {
			return fmt.Errorf("`storage_account_id` can only be specified for a Classic (unmanaged) Sku.")
		}

		parameters.StorageAccount = &containerregistry.StorageAccountProperties{
			ID: utils.String(v.(string)),
		}
	} else if strings.EqualFold(sku, string(containerregistry.Classic)) {
		return fmt.Errorf("`storage_account_id` must be specified for a Classic (unmanaged) Sku.")
	}

	future, err := client.Create(ctx, resourceGroup, name, parameters)
	if err != nil {
		return fmt.Errorf("Error creating Container Registry %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	if err = future.WaitForCompletionRef(ctx, client.Client); err != nil {
		return fmt.Errorf("Error waiting for creation of Container Registry %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	// locations have been specified for geo-replication
	if geoReplicationLocations != nil && geoReplicationLocations.Len() > 0 {
		// the ACR is being created so no previous geo-replication locations
		oldGeoReplicationLocations := []interface{}{}
		err = applyGeoReplicationLocations(d, meta, resourceGroup, name, oldGeoReplicationLocations, geoReplicationLocations.List())
		if err != nil {
			return fmt.Errorf("Error applying geo replications for Container Registry %q (Resource Group %q): %+v", name, resourceGroup, err)
		}
	}

	read, err := client.Get(ctx, resourceGroup, name)
	if err != nil {
		return fmt.Errorf("Error retrieving Container Registry %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	if read.ID == nil {
		return fmt.Errorf("Cannot read Container Registry %q (resource group %q) ID", name, resourceGroup)
	}

	d.SetId(*read.ID)

	return resourceContainerRegistryRead(d, meta)
}

func resourceContainerRegistryUpdate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Containers.RegistriesClient
	ctx, cancel := timeouts.ForUpdate(meta.(*clients.Client).StopContext, d)
	defer cancel()
	log.Printf("[INFO] preparing arguments for  Container Registry update.")

	resourceGroup := d.Get("resource_group_name").(string)
	name := d.Get("name").(string)

	sku := d.Get("sku").(string)
	skuChange := d.HasChange("sku")
	isBasicSku := strings.EqualFold(sku, string(containerregistry.Basic))
	isPremiumSku := strings.EqualFold(sku, string(containerregistry.Premium))
	isStandardSku := strings.EqualFold(sku, string(containerregistry.Standard))

	adminUserEnabled := d.Get("admin_enabled").(bool)
	t := d.Get("tags").(map[string]interface{})

	old, new := d.GetChange("georeplication_locations")
	hasGeoReplicationChanges := d.HasChange("georeplication_locations")
	oldGeoReplicationLocations := old.(*schema.Set)
	newGeoReplicationLocations := new.(*schema.Set)

	// handle upgrade to Premium SKU first
	if skuChange && isPremiumSku {
		if err := applyContainerRegistrySku(d, meta, sku, resourceGroup, name); err != nil {
			return fmt.Errorf("Error applying sku %q for Container Registry %q (Resource Group %q): %+v", sku, name, resourceGroup, err)
		}
	}

	networkRuleSet := expandNetworkRuleSet(d.Get("network_rule_set").([]interface{}))
	if networkRuleSet != nil && isBasicSku {
		return fmt.Errorf("`network_rule_set_set` can only be specified for a Premium Sku. If you are reverting from a Premium to Basic SKU plese set network_rule_set = []")
	}

	retentionPolicy := expandRetentionPolicy(d.Get("retention_policy").([]interface{}))
	trustPolicy := expandTrustPolicy(d.Get("trust_policy").([]interface{}))

	publicNetworkAccess := containerregistry.PublicNetworkAccessEnabled
	if !d.Get("public_network_access_enabled").(bool) {
		publicNetworkAccess = containerregistry.PublicNetworkAccessDisabled
	}

	parameters := containerregistry.RegistryUpdateParameters{
		RegistryPropertiesUpdateParameters: &containerregistry.RegistryPropertiesUpdateParameters{
			AdminUserEnabled: utils.Bool(adminUserEnabled),
			NetworkRuleSet:   networkRuleSet,
			Policies: &containerregistry.Policies{
				RetentionPolicy: retentionPolicy,
				TrustPolicy:     trustPolicy,
			},
			PublicNetworkAccess: publicNetworkAccess,
		},
		Tags: tags.Expand(t),
	}

	// geo replication is only supported by Premium Sku
	if hasGeoReplicationChanges && newGeoReplicationLocations.Len() > 0 && !strings.EqualFold(sku, string(containerregistry.Premium)) {
		return fmt.Errorf("ACR geo-replication can only be applied when using the Premium Sku.")
	}

	// if the registry had replications and is updated to another Sku than premium - remove old locations
	if !strings.EqualFold(sku, string(containerregistry.Premium)) && oldGeoReplicationLocations != nil && oldGeoReplicationLocations.Len() > 0 {
		err := applyGeoReplicationLocations(d, meta, resourceGroup, name, oldGeoReplicationLocations.List(), newGeoReplicationLocations.List())
		if err != nil {
			return fmt.Errorf("Error applying geo replications for Container Registry %q (Resource Group %q): %+v", name, resourceGroup, err)
		}
	}

	future, err := client.Update(ctx, resourceGroup, name, parameters)
	if err != nil {
		return fmt.Errorf("Error updating Container Registry %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	if err = future.WaitForCompletionRef(ctx, client.Client); err != nil {
		return fmt.Errorf("Error waiting for update of Container Registry %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	if strings.EqualFold(sku, string(containerregistry.Premium)) && hasGeoReplicationChanges {
		err = applyGeoReplicationLocations(d, meta, resourceGroup, name, oldGeoReplicationLocations.List(), newGeoReplicationLocations.List())
		if err != nil {
			return fmt.Errorf("Error applying geo replications for Container Registry %q (Resource Group %q): %+v", name, resourceGroup, err)
		}
	}

	// downgrade to Basic or Standard SKU
	if skuChange && (isBasicSku || isStandardSku) {
		if err := applyContainerRegistrySku(d, meta, sku, resourceGroup, name); err != nil {
			return fmt.Errorf("Error applying sku %q for Container Registry %q (Resource Group %q): %+v", sku, name, resourceGroup, err)
		}
	}

	read, err := client.Get(ctx, resourceGroup, name)
	if err != nil {
		return fmt.Errorf("Error retrieving Container Registry %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	if read.ID == nil {
		return fmt.Errorf("Cannot read Container Registry %q (resource group %q) ID", name, resourceGroup)
	}

	d.SetId(*read.ID)

	return resourceContainerRegistryRead(d, meta)
}

func applyContainerRegistrySku(d *schema.ResourceData, meta interface{}, sku string, resourceGroup string, name string) error {
	client := meta.(*clients.Client).Containers.RegistriesClient
	ctx, cancel := timeouts.ForUpdate(meta.(*clients.Client).StopContext, d)
	defer cancel()

	parameters := containerregistry.RegistryUpdateParameters{
		Sku: &containerregistry.Sku{
			Name: containerregistry.SkuName(sku),
			Tier: containerregistry.SkuTier(sku),
		},
	}

	future, err := client.Update(ctx, resourceGroup, name, parameters)
	if err != nil {
		return fmt.Errorf("Error updating Container Registry %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	if err = future.WaitForCompletionRef(ctx, client.Client); err != nil {
		return fmt.Errorf("Error waiting for update of Container Registry %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	return nil
}

func applyGeoReplicationLocations(d *schema.ResourceData, meta interface{}, resourceGroup string, name string, oldGeoReplicationLocations []interface{}, newGeoReplicationLocations []interface{}) error {
	replicationClient := meta.(*clients.Client).Containers.ReplicationsClient
	ctx, cancel := timeouts.ForCreateUpdate(meta.(*clients.Client).StopContext, d)
	defer cancel()
	log.Printf("[INFO] preparing to apply geo-replications for  Container Registry.")

	createLocations := make(map[string]bool)

	// loop on the new location values
	for _, nl := range newGeoReplicationLocations {
		newLocation := azure.NormalizeLocation(nl)
		createLocations[newLocation] = true // the location needs to be created
	}

	// loop on the old location values
	for _, ol := range oldGeoReplicationLocations {
		// oldLocation was created from a previous deployment
		oldLocation := azure.NormalizeLocation(ol)

		// if the list of locations to create already contains the location
		if _, ok := createLocations[oldLocation]; ok {
			createLocations[oldLocation] = false // the location do not need to be created, it already exists
		}
	}

	// create new geo-replication locations
	for locationToCreate := range createLocations {
		// if false, the location does not need to be created, continue
		if !createLocations[locationToCreate] {
			continue
		}

		// create the new replication location
		replication := containerregistry.Replication{
			Location: &locationToCreate,
			Name:     &locationToCreate,
		}

		future, err := replicationClient.Create(ctx, resourceGroup, name, locationToCreate, replication)
		if err != nil {
			return fmt.Errorf("Error creating Container Registry Replication %q (Resource Group %q, Location %q): %+v", name, resourceGroup, locationToCreate, err)
		}

		if err = future.WaitForCompletionRef(ctx, replicationClient.Client); err != nil {
			return fmt.Errorf("Error waiting for creation of Container Registry Replication %q (Resource Group %q, Location %q): %+v", name, resourceGroup, locationToCreate, err)
		}
	}

	// loop on the list of previously deployed locations
	for _, ol := range oldGeoReplicationLocations {
		oldLocation := azure.NormalizeLocation(ol)
		// if the old location is still in the list of locations, then continue
		if _, ok := createLocations[oldLocation]; ok {
			continue
		}

		// the old location is not in the list of locations, delete it
		future, err := replicationClient.Delete(ctx, resourceGroup, name, oldLocation)
		if err != nil {
			return fmt.Errorf("Error deleting Container Registry Replication %q (Resource Group %q, Location %q): %+v", name, resourceGroup, oldLocation, err)
		}

		if err = future.WaitForCompletionRef(ctx, replicationClient.Client); err != nil {
			return fmt.Errorf("Error waiting for deletion of Container Registry Replication %q (Resource Group %q, Location %q): %+v", name, resourceGroup, oldLocation, err)
		}
	}

	return nil
}

func resourceContainerRegistryRead(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Containers.RegistriesClient
	replicationClient := meta.(*clients.Client).Containers.ReplicationsClient
	ctx, cancel := timeouts.ForRead(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := azure.ParseAzureResourceID(d.Id())
	if err != nil {
		return err
	}
	resourceGroup := id.ResourceGroup
	name := id.Path["registries"]

	resp, err := client.Get(ctx, resourceGroup, name)
	if err != nil {
		if utils.ResponseWasNotFound(resp.Response) {
			log.Printf("[DEBUG] Container Registry %q was not found in Resource Group %q", name, resourceGroup)
			d.SetId("")
			return nil
		}

		return fmt.Errorf("Error making Read request on Azure Container Registry %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	d.Set("name", resp.Name)
	d.Set("resource_group_name", resourceGroup)

	location := resp.Location
	if location != nil {
		d.Set("location", azure.NormalizeLocation(*location))
	}
	d.Set("admin_enabled", resp.AdminUserEnabled)
	d.Set("login_server", resp.LoginServer)
	d.Set("public_network_access_enabled", resp.PublicNetworkAccess == containerregistry.PublicNetworkAccessEnabled)

	networkRuleSet := flattenNetworkRuleSet(resp.NetworkRuleSet)
	if err := d.Set("network_rule_set", networkRuleSet); err != nil {
		return fmt.Errorf("Error setting `network_rule_set`: %+v", err)
	}

	if properties := resp.RegistryProperties; properties != nil {
		if err := d.Set("retention_policy", flattenRetentionPolicy(properties.Policies)); err != nil {
			return fmt.Errorf("Error setting `retention_policy`: %+v", err)
		}
		if err := d.Set("trust_policy", flattenTrustPolicy(properties.Policies)); err != nil {
			return fmt.Errorf("Error setting `trust_policy`: %+v", err)
		}
	}

	if sku := resp.Sku; sku != nil {
		d.Set("sku", string(sku.Tier))
	}

	if account := resp.StorageAccount; account != nil {
		d.Set("storage_account_id", account.ID)
	}

	if *resp.AdminUserEnabled {
		credsResp, errList := client.ListCredentials(ctx, resourceGroup, name)
		if errList != nil {
			return fmt.Errorf("Error making Read request on Azure Container Registry %s for Credentials: %s", name, errList)
		}

		d.Set("admin_username", credsResp.Username)
		for _, v := range *credsResp.Passwords {
			d.Set("admin_password", v.Value)
			break
		}
	} else {
		d.Set("admin_username", "")
		d.Set("admin_password", "")
	}

	replications, err := replicationClient.List(ctx, resourceGroup, name)
	if err != nil {
		return fmt.Errorf("Error making Read request on Azure Container Registry %s for replications: %s", name, err)
	}

	replicationValues := replications.Values()

	// if there is more than one location (the main one and the replicas)
	if replicationValues != nil || len(replicationValues) > 1 {
		georeplication_locations := &schema.Set{F: schema.HashString}

		for _, value := range replicationValues {
			if value.Location != nil {
				valueLocation := azure.NormalizeLocation(*value.Location)
				if location != nil && valueLocation != azure.NormalizeLocation(*location) {
					georeplication_locations.Add(valueLocation)
				}
			}
		}

		d.Set("georeplication_locations", georeplication_locations)
	}

	return tags.FlattenAndSet(d, resp.Tags)
}

func resourceContainerRegistryDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Containers.RegistriesClient
	ctx, cancel := timeouts.ForDelete(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := azure.ParseAzureResourceID(d.Id())
	if err != nil {
		return err
	}
	resourceGroup := id.ResourceGroup
	name := id.Path["registries"]

	future, err := client.Delete(ctx, resourceGroup, name)
	if err != nil {
		if response.WasNotFound(future.Response()) {
			return nil
		}
		return fmt.Errorf("Error issuing Azure ARM delete request of Container Registry '%s': %+v", name, err)
	}

	if err = future.WaitForCompletionRef(ctx, client.Client); err != nil {
		if response.WasNotFound(future.Response()) {
			return nil
		}
		return fmt.Errorf("Error issuing Azure ARM delete request of Container Registry '%s': %+v", name, err)
	}

	return nil
}

func ValidateContainerRegistryName(v interface{}, k string) (warnings []string, errors []error) {
	value := v.(string)
	if !regexp.MustCompile(`^[a-zA-Z0-9]+$`).MatchString(value) {
		errors = append(errors, fmt.Errorf(
			"alpha numeric characters only are allowed in %q: %q", k, value))
	}

	if 5 > len(value) {
		errors = append(errors, fmt.Errorf("%q cannot be less than 5 characters: %q", k, value))
	}

	if len(value) >= 50 {
		errors = append(errors, fmt.Errorf("%q cannot be longer than 50 characters: %q %d", k, value, len(value)))
	}

	return warnings, errors
}

func expandNetworkRuleSet(profiles []interface{}) *containerregistry.NetworkRuleSet {
	if len(profiles) == 0 {
		return nil
	}

	profile := profiles[0].(map[string]interface{})

	ipRuleConfigs := profile["ip_rule"].(*schema.Set).List()
	ipRules := make([]containerregistry.IPRule, 0)
	for _, ipRuleInterface := range ipRuleConfigs {
		config := ipRuleInterface.(map[string]interface{})
		newIpRule := containerregistry.IPRule{
			Action:           containerregistry.Action(config["action"].(string)),
			IPAddressOrRange: utils.String(config["ip_range"].(string)),
		}
		ipRules = append(ipRules, newIpRule)
	}

	networkRuleConfigs := profile["virtual_network"].(*schema.Set).List()
	virtualNetworkRules := make([]containerregistry.VirtualNetworkRule, 0)
	for _, networkRuleInterface := range networkRuleConfigs {
		config := networkRuleInterface.(map[string]interface{})
		newVirtualNetworkRule := containerregistry.VirtualNetworkRule{
			Action:                   containerregistry.Action(config["action"].(string)),
			VirtualNetworkResourceID: utils.String(config["subnet_id"].(string)),
		}
		virtualNetworkRules = append(virtualNetworkRules, newVirtualNetworkRule)
	}

	networkRuleSet := containerregistry.NetworkRuleSet{
		DefaultAction:       containerregistry.DefaultAction(profile["default_action"].(string)),
		IPRules:             &ipRules,
		VirtualNetworkRules: &virtualNetworkRules,
	}
	return &networkRuleSet
}

func expandRetentionPolicy(p []interface{}) *containerregistry.RetentionPolicy {
	retentionPolicy := containerregistry.RetentionPolicy{
		Status: containerregistry.PolicyStatusDisabled,
	}

	if len(p) > 0 {
		v := p[0].(map[string]interface{})
		days := int32(v["days"].(int))
		enabled := v["enabled"].(bool)
		if enabled {
			retentionPolicy.Status = containerregistry.PolicyStatusEnabled
		}
		retentionPolicy.Days = utils.Int32(days)
	}

	return &retentionPolicy
}

func expandTrustPolicy(p []interface{}) *containerregistry.TrustPolicy {
	trustPolicy := containerregistry.TrustPolicy{
		Status: containerregistry.PolicyStatusDisabled,
	}

	if len(p) > 0 {
		v := p[0].(map[string]interface{})
		enabled := v["enabled"].(bool)
		if enabled {
			trustPolicy.Status = containerregistry.PolicyStatusEnabled
		}
		trustPolicy.Type = containerregistry.Notary
	}

	return &trustPolicy
}

func flattenNetworkRuleSet(networkRuleSet *containerregistry.NetworkRuleSet) []interface{} {
	if networkRuleSet == nil {
		return []interface{}{}
	}

	values := make(map[string]interface{})

	values["default_action"] = string(networkRuleSet.DefaultAction)

	ipRules := make([]interface{}, 0)
	for _, ipRule := range *networkRuleSet.IPRules {
		value := make(map[string]interface{})
		value["action"] = string(ipRule.Action)

		// When a /32 CIDR is passed as an ip rule, Azure will drop the /32 leading to the resource wanting to be re-created next run
		if !strings.Contains(*ipRule.IPAddressOrRange, "/") {
			*ipRule.IPAddressOrRange += "/32"
		}

		value["ip_range"] = ipRule.IPAddressOrRange
		ipRules = append(ipRules, value)
	}

	values["ip_rule"] = ipRules

	virtualNetworkRules := make([]interface{}, 0)

	if networkRuleSet.VirtualNetworkRules != nil {
		for _, virtualNetworkRule := range *networkRuleSet.VirtualNetworkRules {
			value := make(map[string]interface{})
			value["action"] = string(virtualNetworkRule.Action)

			value["subnet_id"] = virtualNetworkRule.VirtualNetworkResourceID
			virtualNetworkRules = append(virtualNetworkRules, value)
		}
	}

	values["virtual_network"] = virtualNetworkRules

	return []interface{}{values}
}

func flattenRetentionPolicy(p *containerregistry.Policies) []interface{} {
	if p == nil || p.RetentionPolicy == nil {
		return nil
	}

	r := *p.RetentionPolicy
	retentionPolicy := make(map[string]interface{})
	retentionPolicy["days"] = r.Days
	enabled := strings.EqualFold(string(r.Status), string(containerregistry.Enabled))
	retentionPolicy["enabled"] = utils.Bool(enabled)
	return []interface{}{retentionPolicy}
}

func flattenTrustPolicy(p *containerregistry.Policies) []interface{} {
	if p == nil || p.TrustPolicy == nil {
		return nil
	}

	t := *p.TrustPolicy
	trustPolicy := make(map[string]interface{})
	enabled := strings.EqualFold(string(t.Status), string(containerregistry.Enabled))
	trustPolicy["enabled"] = utils.Bool(enabled)
	return []interface{}{trustPolicy}
}
