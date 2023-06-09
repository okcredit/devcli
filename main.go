package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"gopkg.in/yaml.v3"
)

type Connection struct {
	LocalPort  int    `yaml:"local_port"`
	RemoteHost string `yaml:"remote_host"`
	RemotePort int    `yaml:"remote_port"`
}

type Bastion struct {
	Name        string       `yaml:"name"`
	Zone        string       `yaml:"zone"`
	Connections []Connection `yaml:"connections"`
}

type Workload struct {
	Namespace  string `yaml:"namespace"`
	App        string `yaml:"app"`
	LocalPort  int    `yaml:"local_port"`
	RemotePort int    `yaml:"remote_port"`
}

type CloudConfig struct {
	Gcloudconfig string `yaml:"gcloudconfig"`
	Kubeconfig   string `yaml:"kubeconfig"`
}

type ProxyConfig struct {
	Environment  string     `yaml:"environment"`
	CloudProject string     `yaml:"cloud_project"`
	Bastion      Bastion    `yaml:"bastion"`
	Workloads    []Workload `yaml:"workloads"`
}

type Config struct {
	Cloud       CloudConfig   `yaml:"cloud"`
	Proxies     []ProxyConfig `yaml:"proxies"`
	Environment string        `yaml:"environment"`
}

var ErrDuplicateLocalPorts = errors.New("duplicate_local_ports")

func checkKubectl(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "kubectl", "version", "--client")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

func checkGcloud(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "gcloud", "version")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

// validateLocalPorts checks if there are duplicate local ports and returns true if there are duplicate local ports
func validateLocalPorts(config ProxyConfig) ([]int, error) {
	localPorts := make(map[int]bool)

	for _, workload := range config.Workloads {
		if localPorts[workload.LocalPort] {
			fmt.Println("Error: duplicate local ports in the configuration file.", workload.LocalPort)
			return nil, ErrDuplicateLocalPorts
		}
		localPorts[workload.LocalPort] = true
	}

	for _, connection := range config.Bastion.Connections {
		if localPorts[connection.LocalPort] {
			fmt.Println("Error: duplicate local ports in the configuration file.", connection.LocalPort)
			return nil, ErrDuplicateLocalPorts
		}
		localPorts[connection.LocalPort] = true
	}

	// return list of local ports from localPorts map
	var localPortsList []int
	for localPort := range localPorts {
		localPortsList = append(localPortsList, localPort)
	}
	return localPortsList, nil
}

func connectBastion(ctx context.Context, bastion Bastion, connection Connection) *exec.Cmd {
	sshCmd := exec.CommandContext(ctx, "gcloud", "compute", "ssh", bastion.Name, "--zone", bastion.Zone, "--", "-L", fmt.Sprintf("localhost:%d:%s:%d", connection.LocalPort, connection.RemoteHost, connection.RemotePort), "-t")
	sshCmd.Stderr = os.Stderr
	return sshCmd
}

// checkPortAvailable checks if the port on local machine is available
func checkPortAvailable(port int) bool {
	cmd := exec.Command("lsof", "-i", fmt.Sprintf(":%d", port))
	if err := cmd.Run(); err != nil {
		return true
	}
	return false
}

func killProcess(port int) error {
	fmt.Println("Killing the process using port:", port)
	// find the pid for the port
	portCmd := exec.Command("lsof", "-t", fmt.Sprintf("-i:%d", port))
	out, err := portCmd.Output()
	if err != nil {
		return err
	}
	pid := strings.Replace(string(out), "\n", "", -1)
	// kill the process using the pid
	killCmd := exec.Command("kill", "-9", pid)
	if err := killCmd.Run(); err != nil {
		return err
	}
	fmt.Println("Successfully killed the process using port:", port)
	return nil
}

func getPortReuseConfirmation(port int) string {
	fmt.Printf("Error: port %d is being used by another process.\n", port)
	fmt.Println("Do you want to kill the process using this port?")
	fmt.Println(`Warning: If you kill this process, you will not be able to access the application running on this port.`)
	fmt.Println("Please choose one of the action: (a/y/n/e)")
	fmt.Println("a - kill all processes if an existing process is using ports in the configuration file")
	fmt.Println("y - kill the process using this port")
	fmt.Println("n - do not kill the process using this port")
	fmt.Println("e - exit the program")
	var input string
	fmt.Scanln(&input)
	input = strings.TrimSpace(input)
	input = strings.ToLower(input)
	if input != "a" && input != "y" && input != "n" && input != "e" {
		fmt.Println("Invalid input. retry...")
		return getPortReuseConfirmation(port)
	}
	return input
}

func main() {
	// Parse command line arguments
	confFile := flag.String("conf", "", "Path to the configuration file")
	environment := flag.String("env", "", "Environment type (dev, staging, prod)")
	flag.Parse()

	if *confFile == "" {
		// take default configuration file path from home directory
		homeDir, err := os.UserHomeDir()
		if err != nil {
			fmt.Println("Error getting user home directory:", err)
			os.Exit(1)
		}
		*confFile = fmt.Sprintf("%s/.devcli/config.yaml", homeDir)
		// check if default configuration file exists
		if _, err := os.Stat(*confFile); os.IsNotExist(err) {
			// if default configuration file does not exist, create it
			err := os.MkdirAll(fmt.Sprintf("%s/.devcli", homeDir), 0755)
			if err != nil {
				fmt.Println("Error creating default configuration file:", err)
				os.Exit(1)
			}
			// default configuration file content
			defaultConfig := ``
			err = os.WriteFile(*confFile, []byte(defaultConfig), 0644)
			if err != nil {
				fmt.Println("Error writing default configuration file:", err)
				os.Exit(1)
			}
		}
	} else {
		// print configuration file path
		fmt.Println("Using configuration file:", *confFile)
		// check if configuration file exists
		if _, err := os.Stat(*confFile); os.IsNotExist(err) {
			fmt.Println("Error: configuration file does not exist at given path.")
			os.Exit(1)
		}
	}

	// Print devcli program header
	fmt.Println("devcli - Development CLI")
	fmt.Println("Initializing...")

	// Create a context that will be used to cancel the port-forward commands
	// when the program is interrupted
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// check if gcloud is installed and configured
	if !checkGcloud(ctx) {
		fmt.Println("Error: gcloud is not installed or not in the system's PATH.")
		os.Exit(1)
	}

	// Check if kubectl is installed and configured
	if !checkKubectl(ctx) {
		fmt.Println("Error: kubectl is not installed or not in the system's PATH.")
		os.Exit(1)
	}

	// log gcloud version
	cmd := exec.CommandContext(ctx, "gcloud", "version")
	fmt.Println("Using gcloud version:")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		fmt.Println("Error getting gcloud version:", err)
		os.Exit(1)
	}

	// Read and parse the configuration file
	configData, err := os.ReadFile(*confFile)
	if err != nil {
		fmt.Println("Error reading configuration file:", err)
		os.Exit(1)
	}

	var config Config
	err = yaml.Unmarshal(configData, &config)
	if err != nil {
		fmt.Println("Error parsing configuration file:", err)
		os.Exit(1)
	}

	// check if environment is set
	if config.Environment == "" && *environment == "" {
		fmt.Println("Error: environment is not set in the configuration file or passed as a command line argument.")
		os.Exit(1)
	} else if *environment != "" {
		config.Environment = *environment
	}
	fmt.Println("Setting up Environment:", config.Environment)

	// get the proxy configuration for the environment
	var proxyConfig ProxyConfig
	for _, proxy := range config.Proxies {
		if proxy.Environment == config.Environment {
			proxyConfig = proxy
			break
		}
	}
	// print error if proxy configuration is not found
	if proxyConfig.Environment == "" {
		fmt.Println("Error: proxy configuration for environment", config.Environment, "is not found.")
		os.Exit(1)
	}

	// Check if there are duplicate local ports
	localPorts, err := validateLocalPorts(proxyConfig)
	if err == ErrDuplicateLocalPorts {
		fmt.Println("Error: there are duplicate local ports in the configuration file.")
		os.Exit(1)
	}

	var reusePorts bool

	// check if the port on local machine is available
	for _, port := range localPorts {
		if !checkPortAvailable(port) {
			// check if reusePorts is set to true
			if !reusePorts {
				// ask user if they want to reuse ports
				input := getPortReuseConfirmation(port)
				if input == "a" {
					reusePorts = true
				} else if input == "e" {
					fmt.Println("Exiting devcli...")
					os.Exit(1)
				} else if input == "n" {
					continue
				} else if input == "y" {
					// kill the process using the port
					err := killProcess(port)
					if err != nil {
						fmt.Println("Error killing process using port:", err)
						os.Exit(1)
					}
				}
			}
			if reusePorts {
				// kill the process using the port
				err := killProcess(port)
				if err != nil {
					fmt.Println("Error killing process using port:", err)
					os.Exit(1)
				}
			}
		}
	}

	// print when proxy configuration is found
	fmt.Println("Setting up proxy for environment", proxyConfig.Environment)

	// get zone of the bastion instance using gcloud
	cmd = exec.CommandContext(ctx, "gcloud", "compute", "instances", "list", "--filter", fmt.Sprintf("name=%v", proxyConfig.Bastion.Name), "--format", "value(zone)")
	cmd.Stderr = os.Stderr
	zone, err := cmd.Output()
	if err != nil {
		fmt.Println("Error getting zone of the bastion instance:", err)
		os.Exit(1)
	} else {
		proxyConfig.Bastion.Zone = strings.Replace(string(zone), "\n", "", -1)
		fmt.Println("Setting the Zone of the bastion instance:", proxyConfig.Bastion.Zone)
	}

	// Set the KUBECONFIG environment variable
	if config.Cloud.Kubeconfig == "" {
		fmt.Println("kubeconfig is not set in the configuration file.")
		// get default kubeconfig path from home directory
		fmt.Println("Using default kubeconfig path: $HOME/.kube/config")
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Println("Error getting home directory:", err)
			os.Exit(1)
		}
		config.Cloud.Kubeconfig = fmt.Sprintf("%s/.kube/config", home)
	}
	fmt.Println("Using the KUBECONFIG from:", config.Cloud.Kubeconfig)
	os.Setenv("KUBECONFIG", config.Cloud.Kubeconfig)

	gcloudProjectName := proxyConfig.CloudProject
	gcloudConfigPath := config.Cloud.Gcloudconfig

	// Set the CLOUDSDK_CONFIG environment variable
	if gcloudConfigPath == "" {
		fmt.Println("gcloud config path is not set in the configuration file.")
		// get default gcloud config path from home directory
		fmt.Println("Using default gcloud config path: $HOME/.config/gcloud")
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Println("Error getting home directory:", err)
			os.Exit(1)
		}
		gcloudConfigPath = fmt.Sprintf("%s/.config/gcloud", home)
	}
	fmt.Println("Using the gcloud config from:", gcloudConfigPath)
	os.Setenv("CLOUDSDK_CONFIG", gcloudConfigPath)

	// check if the project is set
	if gcloudProjectName == "" {
		fmt.Println("Error: project is not set in the configuration file.")
		os.Exit(1)
	}

	// set gcloud project
	fmt.Println("Setting the gcloud project:", gcloudProjectName)
	cmd = exec.CommandContext(ctx, "gcloud", "config", "set", "project", gcloudProjectName)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		fmt.Println("Error setting gcloud project:", err)
		os.Exit(1)
	}

	// get cluster list and set the first cluster as the default cluster
	var defaultClusterName string
	fmt.Println("Getting the default cluster:")
	cmd = exec.CommandContext(ctx, "gcloud", "container", "clusters", "list", "--format", "value(name)")
	if out, err := cmd.Output(); err != nil {
		fmt.Println("Error getting cluster list:", err)
		os.Exit(1)
	} else {
		defaultClusterName = strings.Replace(string(out), "\n", "", -1)
		fmt.Println("Setting the default cluster:", defaultClusterName)
		cmd = exec.CommandContext(ctx, "gcloud", "config", "set", "container/cluster", defaultClusterName)
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
		if err := cmd.Run(); err != nil {
			fmt.Println("Error setting gcloud cluster:", err)
			os.Exit(1)
		}
	}

	// get cluster region
	var defaultClusterRegion string
	fmt.Println("Getting the default cluster region:")
	cmd = exec.CommandContext(ctx, "gcloud", "container", "clusters", "list", "--format", "value(location)")
	if out, err := cmd.Output(); err != nil {
		fmt.Println("Error getting cluster region:", err)
		os.Exit(1)
	} else {
		defaultClusterRegion = strings.Replace(string(out), "\n", "", -1)
		fmt.Println("Setting the default cluster region:", defaultClusterRegion)
		cmd = exec.CommandContext(ctx, "gcloud", "config", "set", "compute/region", defaultClusterRegion)
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
		if err := cmd.Run(); err != nil {
			fmt.Println("Error setting gcloud region:", err)
			os.Exit(1)
		}
	}

	// set env for gcloud export USE_GKE_GCLOUD_AUTH_PLUGIN=True
	fmt.Println("Setting the environment variable for gcloud auth plugin.")
	os.Setenv("USE_GKE_GCLOUD_AUTH_PLUGIN", "True")

	// get credentials for the default cluster
	fmt.Println("Getting the credentials for the default cluster:", defaultClusterName)
	cmd = exec.CommandContext(ctx, "gcloud", "container", "clusters", "get-credentials", defaultClusterName)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		fmt.Println("Error getting cluster credentials:", err)
		os.Exit(1)
	}
	fmt.Println("Successfully got the credentials for the default cluster.")

	// Print initialization complete
	fmt.Println("Initialization complete.")

	// Listen for SIGINT and SIGTERM signals
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)

	// Cancel the context when the program is interrupted
	go func() {
		<-ch
		fmt.Println("Interrupted. Exiting gracefully...")
		// Cancel the context
		cancel()
		<-ch
		fmt.Println("Interrupted again. Force exiting immediately...")
		os.Exit(1)
	}()

	// Run the kubectl port-forward command for each workload
	var wg sync.WaitGroup
	fmt.Println("Starting the port-forwarding proxy...")
	for _, workload := range proxyConfig.Workloads {
		wg.Add(1)
		go func(workload Workload) {
			defer wg.Done()

			// get first pod using workload name
			var podName string
			fmt.Println("Getting the first pod for workload:", workload.App)
			// get the first running pod for the workload
			cmd := exec.CommandContext(ctx, "kubectl", "get", "pods", "-n", workload.Namespace, "-l", fmt.Sprintf("app=%s", workload.App), "-o", "jsonpath={.items[?(@.status.phase=='Running')].metadata.name}")
			if out, err := cmd.Output(); err != nil {
				fmt.Printf("Error getting pod name for app %s: %v\n", workload.App, err)
			} else {
				podList := strings.Split(strings.Replace(string(out), "\n", "", -1), " ")
				if len(podList) == 0 {
					fmt.Printf("No running pod found for app %s in namespace %s with label app=%s in the cluster.\n", workload.App, workload.Namespace, workload.App)
					return
				} else {
					podName = podList[0]
				}
				if podName == "" {
					fmt.Printf("No running pod found for app %s in namespace %s with label app=%s in the cluster.\n", workload.App, workload.Namespace, workload.App)
					return
				}
				fmt.Printf("Got the first pod for workload %s: %s in namespace %s \n", workload.App, podName, workload.Namespace)
				// run kubectl port-forward
				cmd = exec.CommandContext(ctx, "kubectl", "port-forward", fmt.Sprintf("--namespace=%s", workload.Namespace), podName, fmt.Sprintf("%d:%d", workload.LocalPort, workload.RemotePort))
				cmd.Stderr = os.Stderr
				fmt.Printf("Connecting kubectl port-forward for app %s from remote port %d to local port %d\n", workload.App, workload.RemotePort, workload.LocalPort)
				if err := cmd.Run(); err != nil {
					// If the context was canceled, don't print an error
					if ctx.Err() != nil {
						return
					}
					fmt.Printf("Error running kubectl port-forward for pod %s: %v\n", podName, err)
				}
			}
		}(workload)
	}

	// Connect to the bastion server and forward the connections
	fmt.Println("Starting the bastion server connection proxy...")
	for _, connection := range proxyConfig.Bastion.Connections {
		cmd := connectBastion(ctx, proxyConfig.Bastion, connection)
		fmt.Printf("Connecting to remote host %s via bastion server from remote port %d to local port %d\n", connection.RemoteHost, connection.RemotePort, connection.LocalPort)
		go func(connection Connection) {
			if err := cmd.Run(); err != nil {
				// If the context was canceled, don't print an error
				if ctx.Err() != nil {
					return
				}
				fmt.Printf("Error connecting to the remote host %s via bastion server %s: %v\n", connection.RemoteHost, proxyConfig.Bastion.Name, err)
			}
		}(connection)
	}
	wg.Wait()
}
