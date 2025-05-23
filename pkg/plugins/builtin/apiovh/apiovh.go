package pluginapiovh

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/ovh/configstore"
	"github.com/ovh/go-ovh/ovh"

	"github.com/cneill/utask/engine/values"
	"github.com/cneill/utask/pkg/plugins/builtin/httputil"
	"github.com/cneill/utask/pkg/plugins/taskplugin"
	"github.com/cneill/utask/pkg/utils"
)

// the apiovh plugin performs signed http calls on the OVH public API
var (
	Plugin = taskplugin.New("apiovh", "0.6", exec,
		taskplugin.WithConfig(validConfig, APIOVHConfig{}),
		taskplugin.WithExecutorMetadata(ExecutorMetadata),
		taskplugin.WithResources(resourcesapiovh),
	)
)

// APIOVHConfig holds the configuration needed to run the apiovh plugin
// credentials: key to retrieve credentials from configstore
// method: http method
// path:   http path
// body:   http body (optional)
type APIOVHConfig struct {
	Credentials string `json:"credentials"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Body        string `json:"body,omitempty"`
}

// ovhConfig holds the credentials needed to instantiate
// an OVH API client
type ovhConfig struct {
	Endpoint    string `json:"endpoint"`
	AppKey      string `json:"appKey"`
	AppSecret   string `json:"appSecret"`
	ConsumerKey string `json:"consumerKey"`
}

func validConfig(config interface{}) error {
	cfg := config.(*APIOVHConfig)

	switch cfg.Method {
	case "GET", "POST", "PUT", "DELETE":
	default:
		return fmt.Errorf("unknown method for gw runner: %q", cfg.Method)
	}
	// If the API credentials is a template, try to parse it.
	if !strings.Contains(cfg.Credentials, "{{") {
		ovhCfgStr, err := configstore.GetItemValue(cfg.Credentials)
		if err != nil {
			return fmt.Errorf("can't retrieve credentials from configstore: %s", err)
		}

		var ovhcfg ovhConfig
		if err := json.Unmarshal([]byte(ovhCfgStr), &ovhcfg); err != nil {
			return fmt.Errorf("can't unmarshal ovhConfig from configstore: %s", err)
		}

		if _, err := ovh.NewClient(
			ovhcfg.Endpoint,
			ovhcfg.AppKey,
			ovhcfg.AppSecret,
			ovhcfg.ConsumerKey); err != nil {
			return fmt.Errorf("can't create new OVH client: %s", err)
		}
	} else {
		v := values.NewValues()
		if _, err := v.Apply(cfg.Credentials, nil, ""); err != nil {
			return fmt.Errorf("failed to parse credentials template: %w", err)
		}
	}
	return nil
}

func resourcesapiovh(i interface{}) []string {
	cfg := i.(*APIOVHConfig)
	resources := []string{
		"socket",
	}

	ovhCfgStr, err := configstore.GetItemValue(cfg.Credentials)
	if err != nil {
		return resources
	}

	var ovhcfg ovhConfig
	if err := json.Unmarshal([]byte(ovhCfgStr), &ovhcfg); err != nil {
		return resources
	}

	endpoint := "ovh-eu" // default value
	if ovhcfg.Endpoint != "" {
		endpoint = ovhcfg.Endpoint
	}
	if host, ok := ovh.Endpoints[endpoint]; ok {
		uri, _ := url.Parse(host)
		if uri != nil && uri.Host != "" {
			resources = append(resources, "url:"+uri.Host)
		}
	}
	return resources
}

func exec(stepName string, config interface{}, ctx interface{}) (interface{}, interface{}, error) {
	cfg := config.(*APIOVHConfig)

	ovhCfgStr, err := configstore.GetItemValue(cfg.Credentials)
	if err != nil {
		return nil, nil, fmt.Errorf("can't retrieve credentials from configstore: %s", err)
	}

	var ovhcfg ovhConfig
	if err := json.Unmarshal([]byte(ovhCfgStr), &ovhcfg); err != nil {
		return nil, nil, fmt.Errorf("can't unmarshal ovhConfig from configstore: %s", err)
	}

	cli, err := ovh.NewClient(
		ovhcfg.Endpoint,
		ovhcfg.AppKey,
		ovhcfg.AppSecret,
		ovhcfg.ConsumerKey)
	if err != nil {
		return nil, nil, fmt.Errorf("can't create new OVH client: %s", err)
	}

	var body interface{}
	if cfg.Body != "" {
		reader := bytes.NewReader([]byte(cfg.Body))
		if err := utils.JSONnumberUnmarshal(reader, &body); err != nil {
			return nil, nil, fmt.Errorf("can't unmarshal body: %s", err)
		}
	}

	req, err := cli.NewRequest(cfg.Method, cfg.Path, body, true)
	if err != nil {
		return nil, nil, fmt.Errorf("can't create new request: %s", err)
	}

	resp, err := cli.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("can't execute request: %s", err)
	}

	return httputil.UnmarshalResponse(resp)
}

// ExecutorMetadata generates json schema for the metadata returned by the executor
func ExecutorMetadata() string {
	return taskplugin.NewMetadataSchema().
		WithStatusCode().
		WithHeaders("x-ovh-queryid").
		String()
}
