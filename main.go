package main

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/automationbroker/bundle-lib/apb"
	"github.com/automationbroker/bundle-lib/clients"
	"github.com/automationbroker/bundle-lib/registries"
	"github.com/automationbroker/config"
	"github.com/coreos/go-semver/semver"
	"github.com/ghodss/yaml"
	"k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	csv "github.com/coreos-inc/alm/pkg/api/apis/clusterserviceversion/v1alpha1"
	uic "github.com/coreos-inc/alm/pkg/api/apis/uicatalogentry/v1alpha1"
	flags "github.com/jessevdk/go-flags"
	log "github.com/sirupsen/logrus"
)

const (
	// fqNameRegex - regular expression used when forming FQName.
	fqNameRegex      = "[/.:-]"
	apbOperatorImage = "docker.io/shurley/lostromos:canary"
)

// Args - Command line arguments for the ansible service broker.
type Args struct {
	ConfigFile string `short:"c" long:"config" description:"Config File" default:"/etc/discovery-operator/config.yaml"`
	Interval   string `short:"t" long:"interval" description:"Interval between catalog syncs" default:"5m"`
}

func main() {
	var args Args
	var err error

	if args, err = CreateArgs(); err != nil {
		log.Error(err)
		if flagsErr, ok := err.(*flags.Error); ok && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		} else {
			os.Exit(1)
		}
	}
	interval, err := time.ParseDuration(args.Interval)
	if err != nil {
		panic(err)
	}
	doMain(args)
	t := time.NewTicker(interval)
	for _ = range t.C {
		doMain(args)
	}
}

func doMain(args Args) {
	var err error
	var appConfig *config.Config
	var appRegistry []registries.Registry

	if appConfig, err = config.CreateConfig(args.ConfigFile); err != nil {
		log.Error("Failed to read config file")
		log.Error(err.Error())
		os.Exit(1)
	}

	if appRegistry, err = makeRegistries(appConfig); err != nil {
		log.Error("Failed to parse registries")
		log.Error(err.Error())
		os.Exit(1)
	}

	specs, err := Bootstrap(appRegistry)
	if err != nil {
		log.Error("Bootstrap failed")
		log.Error(err.Error())
		os.Exit(1)
	}

	log.Infof("Discovered %v Service Bundles", len(specs))
	clusterServiceVersions := make([]csv.ClusterServiceVersion, 0)
	customResourceDefinitions := make([]v1beta1.CustomResourceDefinition, 0)
	for _, spec := range specs {
		for _, plan := range spec.Plans {
			log.Infof("Discovered %s(%s)", spec.FQName, plan.Name)
			crd := PlanToCRD(spec, plan)
			customResourceDefinitions = append(customResourceDefinitions, crd)
			clusterServiceVersions = append(clusterServiceVersions, PlanToCSV(plan, spec, crd))
		}
	}
	log.Infof("Translated %v Service Bundles to %v ClusterServiceVersions", len(specs), len(clusterServiceVersions))

	k8scli, err := clients.Kubernetes()
	if err != nil {
		log.Error("Failed to instantiate client")
		log.Error(err.Error())
		os.Exit(1)
	} else {
		log.Info("Client instantiated successfully")
	}

	err = writeSpecsToConfigmap(k8scli, "tectonic-system", "tectonic-ocs", clusterServiceVersions, customResourceDefinitions)
	// err = writeSpecsToConfigmap(k8scli, "openshift-ansible-service-broker", "apbconfigmap", clusterServiceVersions, customResourceDefinitions)
	if err != nil {
		log.Error("Failed to write configmap")
		log.Error(err.Error())
		os.Exit(1)
	}
}

// CreateArgs - Will return the arguments that were passed in to the application
func CreateArgs() (Args, error) {
	args := Args{}

	_, err := flags.Parse(&args)
	if err != nil {
		return args, err
	}
	return args, nil
}

func makeRegistries(appConfig *config.Config) ([]registries.Registry, error) {
	appRegistry := make([]registries.Registry, 0, 5)

	for _, config := range appConfig.GetSubConfigArray("registry") {
		c := registries.Config{
			URL:        config.GetString("url"),
			User:       config.GetString("user"),
			Pass:       config.GetString("pass"),
			Org:        config.GetString("org"),
			Tag:        config.GetString("tag"),
			Type:       config.GetString("type"),
			Name:       config.GetString("name"),
			Images:     config.GetSliceOfStrings("images"),
			Namespaces: config.GetSliceOfStrings("namespaces"),
			Fail:       config.GetBool("fail_on_error"),
			WhiteList:  config.GetSliceOfStrings("white_list"),
			BlackList:  config.GetSliceOfStrings("black_list"),
			AuthType:   config.GetString("auth_type"),
			AuthName:   config.GetString("auth_name"),
			Runner:     config.GetString("runner"),
		}

		reg, err := registries.NewRegistry(c, config.GetString("openshift.namespace"))
		if err != nil {
			log.Errorf(
				"Failed to initialize %v Registry err - %v \n", config.GetString("name"), err)
			return nil, err
		}
		appRegistry = append(appRegistry, reg)
	}

	return appRegistry, nil
}

func Bootstrap(registry []registries.Registry) ([]*apb.Spec, error) {
	log.Info("DiscoveryOperator::Bootstrap")
	var specs []*apb.Spec
	var imageCount int

	// Load Specs for each registry
	registryErrors := []error{}
	for _, r := range registry {
		s, count, err := r.LoadSpecs()
		if err != nil && r.Fail(err) {
			log.Errorf("registry caused bootstrap failure - %v", err)
			return nil, err
		}
		if err != nil {
			log.Warningf("registry: %v was unable to complete bootstrap - %v",
				r.RegistryName, err)
			registryErrors = append(registryErrors, err)
		}
		imageCount += count
		// this will also update the plan id
		addNameAndIDForSpec(s, r.RegistryName())
		specs = append(specs, s...)
	}
	if len(registryErrors) == len(registry) {
		return nil, errors.New("all registries failed on bootstrap")
	}
	specManifest := map[string]*apb.Spec{}
	planNameManifest := map[string]string{}

	for _, s := range specs {
		specManifest[s.ID] = s

		// each of the plans from all of the specs gets its own uuid. even
		// though the names may be the same we want them to be globally unique.
		for _, p := range s.Plans {
			if p.ID == "" {
				log.Errorf("We have a plan that did not get its id generated: %v", p.Name)
				continue
			}
			planNameManifest[p.ID] = p.Name
		}
	}

	apb.AddSecrets(specs)

	return specs, nil
}

func writeSpecsToConfigmap(k8scli *clients.KubernetesClient, namespace string, name string, specs []csv.ClusterServiceVersion, crds []v1beta1.CustomResourceDefinition) error {
	var response *v1.ConfigMap

	packages := []uic.PackageManifest{}
	for _, spec := range specs {
		packages = append(packages, uic.PackageManifest{
			PackageName: spec.ObjectMeta.Name,
			Channels: []uic.PackageChannel{
				uic.PackageChannel{
					Name:           spec.Spec.Maturity,
					CurrentCSVName: spec.ObjectMeta.Name,
				},
			},
		})
	}

	serializedPackages, err := yaml.Marshal(packages)
	if err != nil {
		return err
	}
	serializedSpecs, err := yaml.Marshal(specs)
	if err != nil {
		return err
	}
	serializedCRDs, err := yaml.Marshal(crds)
	if err != nil {
		return err
	}

	cm := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Data: map[string]string{
			"clusterServiceVersions":    string(serializedSpecs),
			"customResourceDefinitions": string(serializedCRDs),
			"packages":                  string(serializedPackages),
		},
	}
	response, err = k8scli.Client.CoreV1().ConfigMaps(namespace).Create(&cm)
	if err != nil {
		existing, err := k8scli.Client.CoreV1().ConfigMaps(namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		response, err = k8scli.Client.CoreV1().ConfigMaps(namespace).Update(mergeCMs(existing, &cm))
		if err != nil {
			return err
		}
	}

	log.Infof("Wrote %v ClusterServiceVersions and %v CustomResourceDefinitions to %s", len(specs), len(crds), response.Name)

	return nil
}

func mergeCMs(cm1 *v1.ConfigMap, cm2 *v1.ConfigMap) *v1.ConfigMap {
	var err error

	cm1CSVs := []csv.ClusterServiceVersion{}
	err = yaml.Unmarshal([]byte(cm1.Data["clusterServiceVersions"]), &cm1CSVs)
	if err != nil {
		panic(err)
	}
	cm2CSVs := []csv.ClusterServiceVersion{}
	err = yaml.Unmarshal([]byte(cm2.Data["clusterServiceVersions"]), &cm2CSVs)
	if err != nil {
		panic(err)
	}

	for _, csv2 := range cm2CSVs {
		found := false
		for j, csv1 := range cm1CSVs {
			if csv1.ObjectMeta.Name == csv2.ObjectMeta.Name {
				log.Infof("Replacing ClusterServiceVersion %s in ConfigMap", csv2.ObjectMeta.Name)
				cm1CSVs[j] = csv2
				found = true
			}
		}
		if !found {
			log.Infof("Adding ClusterServiceVersion %s to ConfigMap", csv2.ObjectMeta.Name)
			cm1CSVs = append(cm1CSVs, csv2)
		}
	}
	serializedCSVs, err := yaml.Marshal(cm1CSVs)
	if err != nil {
		panic(err)
	}
	cm1.Data["clusterServiceVersions"] = string(serializedCSVs)

	cm1CRDs := []v1beta1.CustomResourceDefinition{}
	err = yaml.Unmarshal([]byte(cm1.Data["customResourceDefinitions"]), &cm1CRDs)
	if err != nil {
		panic(err)
	}
	cm2CRDs := []v1beta1.CustomResourceDefinition{}
	err = yaml.Unmarshal([]byte(cm2.Data["customResourceDefinitions"]), &cm2CRDs)
	if err != nil {
		panic(err)
	}

	for _, crd2 := range cm2CRDs {
		found := false
		for j, crd1 := range cm1CRDs {
			if crd1.ObjectMeta.Name == crd2.ObjectMeta.Name {
				log.Infof("Replacing CustomResourceDefinition %s in ConfigMap", crd1.ObjectMeta.Name)
				cm1CRDs[j] = crd2
				found = true
			}
		}
		if !found {
			log.Infof("Adding CustomResourceDefinition %s to ConfigMap", crd2.ObjectMeta.Name)
			cm1CRDs = append(cm1CRDs, crd2)
		}
	}
	serializedCRDs, err := yaml.Marshal(cm1CRDs)
	if err != nil {
		panic(err)
	}
	cm1.Data["customResourceDefinitions"] = string(serializedCRDs)

	cm1Packages := []uic.PackageManifest{}
	err = yaml.Unmarshal([]byte(cm1.Data["packages"]), &cm1Packages)
	if err != nil {
		panic(err)
	}
	cm2Packages := []uic.PackageManifest{}
	err = yaml.Unmarshal([]byte(cm2.Data["packages"]), &cm2Packages)
	if err != nil {
		panic(err)
	}

	for _, package2 := range cm2Packages {
		found := false
		for j, package1 := range cm1Packages {
			if package1.PackageName == package2.PackageName {
				log.Infof("Replacing Package %s in ConfigMap", package1.PackageName)
				cm1Packages[j] = package2
				found = true
			}
		}
		if !found {
			cm1Packages = append(cm1Packages, package2)
			log.Infof("Adding Package %s to ConfigMap", package2.PackageName)
		}
	}
	serializedPackages, err := yaml.Marshal(cm1Packages)
	if err != nil {
		panic(err)
	}
	cm1.Data["packages"] = string(serializedPackages)

	return cm1
}

func addNameAndIDForSpec(specs []*apb.Spec, registryName string) {
	for _, spec := range specs {
		// need to make / a hyphen to allow for global uniqueness
		// but still match spec.

		re := regexp.MustCompile(fqNameRegex)
		spec.FQName = re.ReplaceAllLiteralString(
			fmt.Sprintf("%v-%v", registryName, spec.FQName),
			"-")
		spec.FQName = fmt.Sprintf("%.51v", spec.FQName)
		if strings.HasSuffix(spec.FQName, "-") {
			spec.FQName = spec.FQName[:len(spec.FQName)-1]
		}

		// ID Will be a md5 hash of the fully qualified spec name.
		hasher := md5.New()
		hasher.Write([]byte(spec.FQName))
		spec.ID = hex.EncodeToString(hasher.Sum(nil))

		// update the id on the plans, doing it here avoids looping through the
		// specs array again
		addIDForPlan(spec.Plans, spec.FQName)
	}
}

// addIDForPlan - for each of the plans create a new ID
func addIDForPlan(plans []apb.Plan, FQSpecName string) {

	// need to use the index into the array to actually update the struct.
	for i, plan := range plans {
		//plans[i].ID = uuid.New()
		FQPlanName := fmt.Sprintf("%s-%s", FQSpecName, plan.Name)
		hasher := md5.New()
		hasher.Write([]byte(FQPlanName))
		plans[i].ID = hex.EncodeToString(hasher.Sum(nil))
	}
}

func getAPBMeta(meta map[string]interface{}, key string, fallback string) string {
	switch meta[key] {
	case nil:
		return fallback
	default:
		return fmt.Sprintf("%s", meta[key])
	}
}

func PlanToCSV(plan apb.Plan, spec *apb.Spec, crd v1beta1.CustomResourceDefinition) csv.ClusterServiceVersion {
	var specVersion *semver.Version
	specVersion, err := semver.NewVersion(spec.Version)
	if err != nil {
		specVersion = semver.New(spec.Version + ".0") //TODO
	}

	csvName := fmt.Sprintf("%s.%s", spec.FQName, plan.Name)
	displayName := fmt.Sprintf("%s: %s plan", getAPBMeta(spec.Metadata, "displayName", spec.FQName), plan.Name)
	description := getAPBMeta(spec.Metadata, "longDescription", spec.Description)

	return csv.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       csv.ClusterServiceVersionKind,
			APIVersion: csv.ClusterServiceVersionAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      csvName,
			Namespace: "placeholder",
			Annotations: map[string]string{
				"tectonic-visibility": "ocs", //TODO
			},
		},
		Spec: csv.ClusterServiceVersionSpec{
			DisplayName: displayName,
			Description: description,
			Keywords:    spec.Tags,
			Maintainers: []csv.Maintainer{}, //TODO
			Version:     *specVersion,
			Maturity:    "alpha",
			Provider: csv.AppLink{
				Name: "Automation Broker",
				URL:  "automationbroker.io",
			},
			Links: getLinks(spec.Metadata),
			Icon:  []csv.Icon{getIcon(spec)},
			InstallStrategy: csv.NamedInstallStrategy{
				StrategyName:    "deployment",
				StrategySpecRaw: json.RawMessage(apbDeployment(spec, plan, crd)),
			},
			CustomResourceDefinitions: csv.CustomResourceDefinitions{
				Owned: []csv.CRDDescription{CRDToCSVCRD(crd)},
			},
		},
	}
}

func getLinks(metadata map[string]interface{}) []csv.AppLink {
	links := []csv.AppLink{}

	for k, v := range metadata {
		stringified := fmt.Sprintf("%v", v)
		_, err := url.ParseRequestURI(stringified)
		if err != nil {
			// assume this is not a link
			continue
		}
		links = append(links, csv.AppLink{
			Name: camelToTitle(k),
			URL:  stringified,
		})
	}
	return links
}

func camelToTitle(s string) string {
	ret := []string{}
	for _, r := range s {
		if r == unicode.ToUpper(r) {
			ret = append(ret, " ")
		}
		ret = append(ret, string(r))
	}
	return strings.Title(strings.Join(ret, ""))
}

func getIcon(spec *apb.Spec) csv.Icon {
	return csv.Icon{
		Data:      "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",
		MediaType: "image/png",
	}
}

// TODO: needs to be plural
func CRDToCSVCRD(crd v1beta1.CustomResourceDefinition) csv.CRDDescription {
	return csv.CRDDescription{
		Name:            crd.ObjectMeta.Name,
		DisplayName:     crd.Spec.Names.Kind,
		Version:         crd.Spec.Version,
		Kind:            crd.Spec.Names.Kind,
		SpecDescriptors: JSONSchemasToSpecDescriptors(crd.Spec.Validation.OpenAPIV3Schema),
	}
}

func JSONSchemasToSpecDescriptors(schema *v1beta1.JSONSchemaProps) []csv.SpecDescriptor {
	descriptors := []csv.SpecDescriptor{}
	for name, param := range schema.Properties {
		descriptors = append(descriptors, csv.SpecDescriptor{
			Path:        name,
			DisplayName: param.Title,
			Description: param.Description,
			// Value: param.Default, TODO
		})
	}
	return descriptors
}

func PlanToCRD(spec *apb.Spec, plan apb.Plan) v1beta1.CustomResourceDefinition {
	resourceName := strings.ToLower(fmt.Sprintf("%s-%s", spec.FQName, plan.Name))
	parts := strings.Split(resourceName, "-")
	for i, part := range parts {
		parts[i] = strings.Title(part)
	}
	kind := strings.Join(parts, "-")
	plural := resourceName + "s"
	group := "bundle.automationbroker.io"
	version := "v1"

	return v1beta1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1beta1",
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s.%s", plural, group),
		},
		Spec: v1beta1.CustomResourceDefinitionSpec{
			Group:   group,
			Version: version,
			Names: v1beta1.CustomResourceDefinitionNames{
				Kind:     kind,
				Plural:   plural,
				Singular: resourceName,
			},
			Scope: v1beta1.NamespaceScoped,
			Validation: &v1beta1.CustomResourceValidation{
				OpenAPIV3Schema: PlanToJSONSchema(plan),
			},
		},
		Status: v1beta1.CustomResourceDefinitionStatus{},
	}
}

func PlanToJSONSchema(plan apb.Plan) *v1beta1.JSONSchemaProps {
	props := v1beta1.JSONSchemaProps{
		Type:                 "object",
		Description:          plan.Description,
		AdditionalProperties: &v1beta1.JSONSchemaPropsOrBool{Allows: false},
		Properties:           parametersToJSONSchema(plan.Parameters),
		Required:             []string{},
	}
	for _, param := range plan.Parameters {
		if param.Required {
			props.Required = append(props.Required, param.Name)
		}
	}
	return &props
}

func parametersToJSONSchema(params []apb.ParameterDescriptor) map[string]v1beta1.JSONSchemaProps {
	properties := make(map[string]v1beta1.JSONSchemaProps)

	for _, param := range params {
		k := param.Name

		t := getType(param.Type)

		tmpProps := v1beta1.JSONSchemaProps{
			Title:       param.Title,
			Description: param.Description,
			Type:        t,
		}
		setStringValidators(param, &tmpProps)
		// setNumberValidators(param, properties[k])
		// setEnum(param, properties[k])
		properties[k] = tmpProps
	}

	return properties
}

func setStringValidators(pd apb.ParameterDescriptor, prop *v1beta1.JSONSchemaProps) {
	// maxlength
	if pd.DeprecatedMaxlength > 0 {
		tmp := int64(pd.MaxLength)
		prop.MaxLength = &tmp
	}

	// max_length overrides maxlength
	if pd.MaxLength > 0 {
		tmp := int64(pd.MaxLength)
		prop.MaxLength = &tmp
	}
	// min_length
	if pd.MinLength > 0 {
		tmp := int64(pd.MinLength)
		prop.MinLength = &tmp
	}

	// do not set the regexp if it does not compile
	if pd.Pattern != "" {
		prop.Pattern = pd.Pattern
	}
}

// getType transforms an apb parameter type to a JSON Schema type
func getType(paramType string) string {
	return "string"
}

func apbDeployment(spec *apb.Spec, plan apb.Plan, crd v1beta1.CustomResourceDefinition) string {
	apbDeploymentTmpl := `permissions:
- serviceAccountName: {{.name}}-operator
  rules:
  - apiGroups: ['*']
    attributeRestrictions: null
    resources: ['*']
    verbs: ['*']
  - apiGroups:
    - {{.crdGroup}}
    resources:
    - {{.crdName}}
    verbs: ['*']
deployments:
- name: {{.name}}-operator
  spec:
    replicas: 1
    selector:
      matchLabels:
        name: {{.name}}-operator-alm-owned
    template:
      metadata:
        name: {{.name}}-operator-alm-owned
        labels:
          name: {{.name}}-operator-alm-owned
          purpose: lostromos
      spec:
        serviceAccountName: {{.name}}-operator
        containers:
        - name: lostromos
          image: {{.image}}
          imagePullPolicy: Always
          env:
          - name: MY_POD_NAMESPACE
            valueFrom:
              fieldRef:
                fieldPath: metadata.namespace
          args:
          - start
          - "--debug"
          - "--crd-name"
          - "{{.crdName}}"
          - "--crd-group"
          - "{{.crdGroup}}"
          - "--bundle-sandbox-role"
          - "admin"
          - "--bundle-plan"
          - "{{.bundlePlan}}"
          - "--bundle-spec"
          - "{{.b64Spec}}"
          restartPolicy: OnFailure`
	b := &bytes.Buffer{}
	serializedSpec, _ := json.Marshal(spec)
	params := map[string]interface{}{
		"image":      apbOperatorImage,
		"name":       crd.Spec.Names.Singular,
		"crdName":    crd.Spec.Names.Plural,
		"crdGroup":   crd.Spec.Group,
		"bundlePlan": plan.Name,
		"b64Spec":    base64.StdEncoding.EncodeToString(serializedSpec),
	}
	template.Must(template.New("").Parse(apbDeploymentTmpl)).Execute(b, params)
	y, err := yaml.YAMLToJSON(b.Bytes())
	if err != nil {
		panic(err) //TODO
	}
	return string(y)
}
