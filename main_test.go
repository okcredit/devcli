package main

import (
	"context"
	"os/exec"
	"strconv"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCheckKubectl(t *testing.T) {
	// context
	ctx := context.Background()
	result := checkKubectl(ctx)
	if !result {
		t.Error("checkKubectl failed: kubectl is not installed or not in the system's PATH.")
	}
}

// test for check gcloud
func TestCheckGcloud(t *testing.T) {
	// context
	ctx := context.Background()
	result := checkGcloud(ctx)
	if !result {
		t.Error("checkGcloud failed: gcloud is not installed or not in the system's PATH.")
	}
}

func TestValidateLocalPorts(t *testing.T) {
	configData := `
environment: staging

cloud:
  kubeconfig: /path/to/your/kubeconfig.yaml
  gcloudconfig: /path/to/your/gcloudconfig.yaml

proxies:
  - proxy:
    environment: staging
    cloud_project: okcredit-staging-env
    bastion:
      name: bastion
      connections:
        - local_port: 5435
          remote_host: 10.120.52.48
          remote_port: 5432
        - local_port: 5434
          remote_host: 10.116.48.59
          remote_port: 5432
        - local_port: 6378
          remote_host: 10.116.50.3
          remote_port: 6379
    workloads:
      - namespace: enr
        app: cashfree
        local_port: 8080
        remote_port: 8080
  - proxy:
    environment: production
    cloud_project: okcredit-42
    bastion:
      name: bastion
      connections:
        - local_port: 5435
          remote_host: 10.120.49.38
          remote_port: 5432
    workloads:
      - namespace: enr
        app: cashfree
        local_port: 8080
        remote_port: 8080
`

	var config Config
	err := yaml.Unmarshal([]byte(configData), &config)
	if err != nil {
		t.Fatalf("Error parsing configuration data for TestValidateLocalPorts: %v", err)
	}

	// get the proxy configuration for the environment
	var proxyConfig ProxyConfig
	for _, proxy := range config.Proxies {
		if proxy.Environment == config.Environment {
			proxyConfig = proxy
			break
		}
	}

	_, err = validateLocalPorts(proxyConfig)
	if err == ErrDuplicateLocalPorts {
		t.Error("checkDuplicateLocalPorts failed: found duplicate local ports.")
	}
}

func TestConnectBastion(t *testing.T) {
	bastion := Bastion{Name: "bastion"}
	connection := Connection{
		LocalPort:  5434,
		RemoteHost: "10.116.48.59",
		RemotePort: 5432,
	}

	// context
	ctx := context.Background()

	cmd := connectBastion(ctx, bastion, connection)
	// get gcloud path
	gcloudPath, err := exec.LookPath("gcloud")
	if err != nil {
		t.Fatalf("Error getting gcloud path: %v", err)
	}
	if cmd == nil || cmd.Path != gcloudPath {
		t.Error("connectBastion failed: the command is not properly configured.")
	}
}

// test for checkPortAvailable
func TestCheckPortAvailable(t *testing.T) {
	port := 5555
	result := checkPortAvailable(port)
	if !result {
		t.Error("checkPortAvailable failed: the port is not available.")
	}
}

// test for killProcess, killing process by port
func TestKillProcess(t *testing.T) {
	port := 5555
	// create a dummy process at given port
	cmd := exec.Command("nc", "-l", strconv.Itoa(port))
	err := cmd.Start()
	if err != nil {
		t.Fatalf("Error creating dummy process: %v", err)
	}
	err = killProcess(port)
	if err != nil {
		t.Errorf("killProcess failed: %v", err)
	}
}
