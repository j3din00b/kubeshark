package config

import (
	"os"
	"path"
	"path/filepath"

	"github.com/kubeshark/kubeshark/config/configStructs"
	"github.com/kubeshark/kubeshark/misc"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/homedir"
)

const (
	SelfNamespaceConfigName   = "selfnamespace"
	ConfigFilePathCommandName = "configpath"
	KubeConfigPathConfigName  = "kube-configpath"
)

func CreateDefaultConfig() ConfigStruct {
	return ConfigStruct{}
}

type KubeConfig struct {
	ConfigPathStr string `yaml:"configpath"`
	Context       string `yaml:"context"`
}

type ConfigStruct struct {
	Tap            configStructs.TapConfig    `yaml:"tap"`
	Logs           configStructs.LogsConfig   `yaml:"logs"`
	Config         configStructs.ConfigConfig `yaml:"config,omitempty"`
	Kube           KubeConfig                 `yaml:"kube"`
	SelfNamespace  string                     `yaml:"selfnamespace" default:"kubeshark"`
	DumpLogs       bool                       `yaml:"dumplogs" default:"false"`
	ConfigFilePath string                     `yaml:"configpath,omitempty" readonly:""`
	HeadlessMode   bool                       `yaml:"headless" default:"false"`
}

func (config *ConfigStruct) SetDefaults() {
	config.ConfigFilePath = path.Join(misc.GetDotFolderPath(), "config.yaml")
}

func (config *ConfigStruct) ImagePullPolicy() v1.PullPolicy {
	return v1.PullPolicy(config.Tap.Docker.ImagePullPolicy)
}

func (config *ConfigStruct) IsNsRestrictedMode() bool {
	return config.SelfNamespace != misc.Program // Notice "kubeshark" string must match the default SelfNamespace
}

func (config *ConfigStruct) KubeConfigPath() string {
	if config.Kube.ConfigPathStr != "" {
		return config.Kube.ConfigPathStr
	}

	envKubeConfigPath := os.Getenv("KUBECONFIG")
	if envKubeConfigPath != "" {
		return envKubeConfigPath
	}

	home := homedir.HomeDir()
	return filepath.Join(home, ".kube", "config")
}
