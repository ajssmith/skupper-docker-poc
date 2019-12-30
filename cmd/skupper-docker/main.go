package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	dockerfilters "github.com/docker/docker/api/types/filters"
	dockermounttypes "github.com/docker/docker/api/types/mount"
	dockernetworktypes "github.com/docker/docker/api/types/network"
	dockerapi "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"golang.org/x/net/context"

	"github.com/skupperproject/skupper-cli/pkg/certs"
	// note: change this when moved to skupperproject
	"github.com/ajssmith/skupper-docker/pkg/dockershim/libdocker"
	"github.com/spf13/cobra"
)

var (
	version                 = "undefined"
	hostPath                = "/tmp/skupper"
	skupperCertPath         = hostPath + "/qpid-dispatch-certs/"
	skupperConnPath         = hostPath + "/connections/"
	skupperConsoleUsersPath = hostPath + "/console-users/"
	skupperSaslConfigPath   = hostPath + "/sasl-config/"
)

type RouterMode string

const (
	RouterModeInterior RouterMode = "interior"
	RouterModeEdge                = "edge"
)

type ConnectorRole string

const (
	ConnectorRoleInterRouter ConnectorRole = "inter-router"
	ConnectorRoleEdge                      = "edge"
)

func connectJson() string {
	connect_json := `
{
    "scheme": "amqps",
    "host": "skupper-router",
    "port": "5671",
    "tls": {
        "ca": "/etc/messaging/ca.crt",
        "cert": "/etc/messaging/tls.crt",
        "key": "/etc/messaging/tls.key",
        "verify": true
    }
}
`
	return connect_json
}

type ConsoleAuthMode string

const (
	ConsoleAuthModeInternal  ConsoleAuthMode = "internal"
	ConsoleAuthModeUnsecured                 = "unsecured"
)

type Router struct {
	Name            string
	Mode            RouterMode
	Console         ConsoleAuthMode
	ConsoleUser     string
	ConsolePassword string
}

type Bridge struct {
	Name     string `json:"name"`
	Port     string `json:"port"`
	Process  string `json:"process"`
	Protocol string `json:"protocol"`
}

type DockerDetails struct {
	Cli *dockerapi.Client
	Ctx context.Context
}

func routerConfig(router *Router) string {
	config := `
router {
    mode: {{.Mode}}
    id: {{.Name}}-${HOSTNAME}
}

listener {
    host: localhost
    port: 5672
    role: normal
}

sslProfile {
    name: skupper-amqps
    certFile: /etc/qpid-dispatch-certs/skupper-amqps/tls.crt
    privateKeyFile: /etc/qpid-dispatch-certs/skupper-amqps/tls.key
    caCertFile: /etc/qpid-dispatch-certs/skupper-amqps/ca.crt
}

listener {
    host: 0.0.0.0
    port: 5671
    role: normal
    sslProfile: skupper-amqps
    saslMechanisms: ANONYMOUS PLAIN
    authenticatePeer: false
}

{{- if eq .Console "openshift"}}
# console secured by oauth proxy sidecar
listener {
    host: localhost
    port: 8888
    role: normal
    http: true
}
{{- else if eq .Console "internal"}}
listener {
    host: 0.0.0.0
    port: 8080
    role: normal
    http: true
    authenticatePeer: true
}
{{- else if eq .Console "unsecured"}}
listener {
    host: 0.0.0.0
    port: 8080
    role: normal
    http: true
}
{{- end }}

listener {
    host: 0.0.0.0
    port: 9090
    role: normal
    http: true
    httpRootDir: disabled
    websockets: false
    healthz: true
    metrics: true
}

{{- if eq .Mode "interior" }}
sslProfile {
    name: skupper-internal
    certFile: /etc/qpid-dispatch-certs/skupper-internal/tls.crt
    privateKeyFile: /etc/qpid-dispatch-certs/skupper-internal/tls.key
    caCertFile: /etc/qpid-dispatch-certs/skupper-internal/ca.crt
}

listener {
    role: inter-router
    host: 0.0.0.0
    port: 55671
    sslProfile: skupper-internal
    saslMechanisms: EXTERNAL
    authenticatePeer: true
}

listener {
    role: edge
    host: 0.0.0.0
    port: 45671
    sslProfile: skupper-internal
    saslMechanisms: EXTERNAL
    authenticatePeer: true
}
{{- end}}

address {
    prefix: mc
    distribution: multicast
}

## Connectors: ##
`
	var buff bytes.Buffer
	qdrconfig := template.Must(template.New("qdrconfig").Parse(config))
	qdrconfig.Execute(&buff, router)
	return buff.String()
}

type Connector struct {
	Name string
	Host string
	Port string
	Role ConnectorRole
	Cost int
}

func connectorConfig(connector *Connector) string {
	config := `

sslProfile {
    name: {{.Name}}-profile
    certFile: /etc/qpid-dispatch/connections/{{.Name}}/tls.crt
    privateKeyFile: /etc/qpid-dispatch/connections/{{.Name}}/tls.key
    caCertFile: /etc/qpid-dispatch/connections/{{.Name}}/ca.crt
}

connector {
    name: {{.Name}}-connector
    host: {{.Host}}
    port: {{.Port}}
    role: {{.Role}}
    cost: {{.Cost}}
    sslProfile: {{.Name}}-profile
}

`
	var buff bytes.Buffer
	connectorconfig := template.Must(template.New("connectorconfig").Parse(config))
	connectorconfig.Execute(&buff, connector)
	return buff.String()
}

func ensureSaslUsers(user string, password string) {
	fmt.Println("Skupper console path: ", skupperConsoleUsersPath)
	fmt.Println("Skupper console user file: ", skupperConsoleUsersPath+user)
	_, err := ioutil.ReadFile(skupperConsoleUsersPath + user)
	if err == nil {
		fmt.Println("console user already exists")
	} else {
		if err := ioutil.WriteFile(skupperConsoleUsersPath+user, []byte(password), 0755); err != nil {
			log.Fatal("Failed to write console user password file: ", err.Error())
		}
	}
}

func ensureSaslConfig() {
	name := "qdrouterd.conf"

	_, err := ioutil.ReadFile(skupperSaslConfigPath + name)
	if err == nil {
		fmt.Println("sasl config already exists")
	} else {
		config := `
pwcheck_method: auxprop
auxprop_plugin: sasldb
sasldb_path: /tmp/qdrouterd.sasldb
`
		if err := ioutil.WriteFile(skupperSaslConfigPath+name, []byte(config), 0755); err != nil {
			log.Fatal("Failed to write sasl config file: ", err.Error())
		}
	}
}

func routerPorts(router *Router) nat.PortSet {
	ports := nat.PortSet{}

	ports["9090/tcp"] = struct{}{}
	if router.Console != "" {
		ports["8080/tcp"] = struct{}{}
	}
	if router.Mode == RouterModeInterior {
		// inter-router
		ports["55671/tcp"] = struct{}{}
		// edge
		ports["45671/tcp"] = struct{}{}
	}
	return ports
}

func findEnvVar(env []string, name string) string {
	for _, v := range env {
		if strings.HasPrefix(v, name) {
			return strings.TrimPrefix(v, name+"=")
		}
	}
	return ""
}

func setEnvVar(current []string, name string, value string) []string {
	updated := []string{}
	for _, v := range current {
		if strings.HasPrefix(v, name) {
			updated = append(updated, name+"="+value)
		} else {
			updated = append(updated, v)
		}
	}
	return updated
}

func isInterior(tcj *dockertypes.ContainerJSON) bool {
	config := findEnvVar(tcj.Config.Env, "QDROUTERD_CONF")
	if config == "" {
		log.Fatal("Could not retrieve router config")
	}
	match, _ := regexp.MatchString("mode:[ ]+interior", config)
	return match
}

func generateConnectorName(path string) string {
	files, err := ioutil.ReadDir(path)
	max := 1
	if err == nil {
		connectorNamePattern := regexp.MustCompile("conn([0-9])+")
		for _, f := range files {
			count := connectorNamePattern.FindStringSubmatch(f.Name())
			if len(count) > 1 {
				v, _ := strconv.Atoi(count[1])
				if v >= max {
					max = v + 1
				}
			}
		}
	} else {
		log.Fatal("Could not retrieve configured connectors (need init?): ", err.Error())
	}
	return "conn" + strconv.Itoa(max)
}

func getRouterMode(tcj *dockertypes.ContainerJSON) RouterMode {
	if isInterior(tcj) {
		return RouterModeInterior
	} else {
		return RouterModeEdge
	}
}

func routerEnv(router *Router) []string {
	envVars := []string{}

	if router.Mode == RouterModeInterior {
		envVars = append(envVars, "APPLICATION_NAME=skupper-router")
	}
	envVars = append(envVars, "QDROUTERD_CONF="+routerConfig(router))
	envVars = append(envVars, "PN_TRACE_FRM=1")
	if router.Console == ConsoleAuthModeInternal {
		envVars = append(envVars, "QDROUTERD_AUTO_CREATE_SASLDB_SOURCE=/etc/qpid-dispatch/sasl-users/")
		envVars = append(envVars, "QDROUTERD_AUTO_CREATE_SASLDB_PATH=/tmp/qdrouterd.sasldb")
	}

	return envVars
}

func syncEnv(name string) []string {
	envVars := []string{}
	if name != "" {
		envVars = append(envVars, "ICPROXY_BRIDGE_HOST="+name)
	}
	return envVars
}

func proxyEnv(bridge Bridge) []string {
	envVars := []string{}
	if bridge.Process != "" {
		envVars = append(envVars, "ICPROXY_BRIDGE_HOST="+bridge.Process)
	}
	return envVars
}

func getLabels(component string, bridge Bridge) map[string]string {
	application := "skupper"
	if component == "router" {
		//the automeshing function of the router image expects the application
		//to be used as a unique label for identifying routers to connect to
		return map[string]string{
			"application":          "skupper-router",
			"skupper.io/component": component,
			"prometheus.io/port":   "9090",
			"prometheus.io/scrape": "true",
		}
	} else if component == "proxy" {
		return map[string]string{
			"application":          "skupper-proxy",
			"skupper.io/component": component,
			"skupper.io/process":   bridge.Process,
		}
	} else if component == "network" {
		return map[string]string{
			"application":          "skupper-network",
			"skupper.io/component": component,
		}
	}
	return map[string]string{
		"application": application,
	}
}

func randomId(length int) string {
	buffer := make([]byte, length)
	rand.Read(buffer)
	result := base64.StdEncoding.EncodeToString(buffer)
	return result[:length]
}

func getCertData(name string) (certs.CertificateData, error) {
	certData := certs.CertificateData{}
	certPath := skupperCertPath + name

	files, err := ioutil.ReadDir(certPath)
	if err == nil {
		for _, f := range files {
			dataString, err := ioutil.ReadFile(certPath + "/" + f.Name())
			if err == nil {
				certData[f.Name()] = []byte(dataString)
			} else {
				log.Fatal("Failed to read certificat data: ", err.Error())
			}
		}
	}

	return certData, err
}

func ensureCA(name string) certs.CertificateData {
	// check if existing by looking at path/dir, if not create dir to persist
	caData := certs.GenerateCACertificateData(name, name)

	if err := os.Mkdir(skupperCertPath+name, 0755); err != nil {
		log.Fatal("Failed to create certificate directory: ", err.Error())
	}

	for k, v := range caData {
		if err := ioutil.WriteFile(skupperCertPath+name+"/"+k, v, 0755); err != nil {
			log.Fatal("Failed to write CA certificate file: ", err.Error())
		}
	}

	return caData
}

func generateCredentials(caData certs.CertificateData, name string, subject string, hosts string, includeConnectJson bool) {
	certData := certs.GenerateCertificateData(name, subject, hosts, caData)

	for k, v := range certData {
		if err := ioutil.WriteFile(skupperCertPath+name+"/"+k, v, 0755); err != nil {
			log.Fatal("Failed to write certificate file: ", err.Error())
		}
	}

	if includeConnectJson {
		certData["connect.json"] = []byte(connectJson())
		if err := ioutil.WriteFile(skupperCertPath+name+"/connect.json", []byte(connectJson()), 0755); err != nil {
			log.Fatal("Failed to write connect file: ", err.Error())
		}
	}

}

func createRouterHostFiles(volumes []string) {

	// only called during init, remove previous files and start anew
	_ = os.RemoveAll(hostPath)
	if err := os.MkdirAll(hostPath, 0755); err != nil {
		log.Fatal("Failed to create skupper host directory: ", err.Error())
	}
	if err := os.Mkdir(skupperCertPath, 0755); err != nil {
		log.Fatal("Failed to create skupper host directory: ", err.Error())
	}
	if err := os.Mkdir(skupperConnPath, 0755); err != nil {
		log.Fatal("Failed to create skupper host directory: ", err.Error())
	}
	if err := os.Mkdir(skupperConsoleUsersPath, 0777); err != nil {
		log.Fatal("Failed to create skupper host directory: ", err.Error())
	}
	if err := os.Mkdir(skupperSaslConfigPath, 0755); err != nil {
		log.Fatal("Failed to create skupper host directory: ", err.Error())
	}
	for _, v := range volumes {
		if err := os.Mkdir(skupperCertPath+v, 0755); err != nil {
			log.Fatal("Failed to create skupper host directory: ", err.Error())
		}
	}

}

func routerHostConfig(router *Router) *dockercontainer.HostConfig {

	hostcfg := &dockercontainer.HostConfig{
		Mounts: []dockermounttypes.Mount{
			{
				Type:   dockermounttypes.TypeBind,
				Source: skupperConnPath,
				Target: "/etc/qpid-dispatch/connections",
			},
			{
				Type:   dockermounttypes.TypeBind,
				Source: skupperCertPath,
				Target: "/etc/qpid-dispatch-certs",
			},
			{
				Type:   dockermounttypes.TypeBind,
				Source: skupperConsoleUsersPath,
				Target: "/etc/qpid-dispatch/sasl-users/",
			},
			{
				Type:   dockermounttypes.TypeBind,
				Source: skupperSaslConfigPath,
				Target: "/etc/sasl2",
			},
		},
		Privileged: true,
	}
	return hostcfg
}

func routerContainerConfig(router *Router) *dockercontainer.Config {
	var image string

	labels := getLabels("router", Bridge{})

	if os.Getenv("QDROUTERD_IMAGE") != "" {
		image = os.Getenv("QDROUTERD_IMAGE")
	} else {
		image = "quay.io/interconnectedcloud/qdrouterd"
	}
	cfg := &dockercontainer.Config{
		Hostname: "skupper-router",
		Image:    image,
		Env:      routerEnv(router),
		Healthcheck: &dockercontainer.HealthConfig{
			Test:        []string{"curl --fail -s http://localhost:9090/healthz || exit 1"},
			StartPeriod: time.Duration(60),
		},
		Labels:       labels,
		ExposedPorts: routerPorts(router),
	}

	return cfg
}

func routerNetworkConfig() *dockernetworktypes.NetworkingConfig {
	netcfg := &dockernetworktypes.NetworkingConfig{
		EndpointsConfig: map[string]*dockernetworktypes.EndpointSettings{
			"skupper-network": {},
		},
	}

	return netcfg
}

func makeRouterContainerCreateConfig(router *Router) *dockertypes.ContainerCreateConfig {

	opts := &dockertypes.ContainerCreateConfig{
		Name:             "skupper-router",
		Config:           routerContainerConfig(router),
		HostConfig:       routerHostConfig(router),
		NetworkingConfig: routerNetworkConfig(),
	}

	return opts
}

func restartRouterContainer(dd libdocker.Interface) {

	current, err := dd.InspectContainer("skupper-router")
	if err != nil {
		log.Fatal("Failed to retrieve router container (need init?): ", err.Error())
	} else {
		mounts := []dockermounttypes.Mount{}
		for _, v := range current.Mounts {
			mounts = append(mounts, dockermounttypes.Mount{
				Type:   v.Type,
				Source: v.Source,
				Target: v.Destination,
			})
		}
		hostCfg := &dockercontainer.HostConfig{
			Mounts:     mounts,
			Privileged: true,
		}

		// grab the env and add connectors to it, splice off current ones
		currentEnv := current.Config.Env
		pattern := "## Connectors: ##"
		qdrConf := findEnvVar(currentEnv, "QDROUTERD_CONF")
		updated := strings.Split(qdrConf, pattern)[0] + pattern

		files, err := ioutil.ReadDir(skupperConnPath)
		for _, f := range files {
			connName := f.Name()
			connector := Connector{
				Name: connName,
			}
			hostString, _ := ioutil.ReadFile(skupperConnPath + connName + "/inter-router-host")
			portString, _ := ioutil.ReadFile(skupperConnPath + connName + "/inter-router-port")
			connector.Host = string(hostString)
			connector.Port = string(portString)
			connector.Role = ConnectorRoleInterRouter
			updated += connectorConfig(&connector)
		}

		newEnv := setEnvVar(currentEnv, "QDROUTERD_CONF", updated)

		containerCfg := &dockercontainer.Config{
			Hostname: current.Config.Hostname,
			Image:    current.Config.Image,
			Healthcheck: &dockercontainer.HealthConfig{
				Test:        []string{"curl --fail -s http://localhost:9090/healthz || exit 1"},
				StartPeriod: time.Duration(60),
			},
			Labels:       current.Config.Labels,
			ExposedPorts: current.Config.ExposedPorts,
			Env:          newEnv,
		}

		// remove current and create new container
		err = dd.StopContainer("skupper-router", 10*time.Second)
		if err == nil {
			if err := dd.RemoveContainer("skupper-router", dockertypes.ContainerRemoveOptions{}); err != nil {
				log.Fatal("Failed to remove router container: ", err.Error())
			}
		}

		opts := &dockertypes.ContainerCreateConfig{
			Name:             "skupper-router",
			Config:           containerCfg,
			HostConfig:       hostCfg,
			NetworkingConfig: routerNetworkConfig(),
		}
		_, err = dd.CreateContainer(*opts)
		if err != nil {
			log.Fatal("Failed to re-create router container: ", err.Error())
		}

		err = dd.StartContainer(opts.Name)
		if err != nil {
			log.Fatal("Failed to re-start router container: ", err.Error())
		}

		err = dd.RestartContainer("skupper-service-sync", 10*time.Second)
		if err != nil {
			log.Fatal("Failed to re-start sercie sync container: ", err.Error())
		}
		restartProxies(dd)
	}

}

func generateConnectSecret(subject string, secretFile string) {
	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)

	// verify that the local deployment is interior
	current, err := dd.InspectContainer("skupper-router")
	if err == nil {
		if isInterior(current) {
			caData, err := getCertData("skupper-internal-ca")
			if err == nil {
				annotations := make(map[string]string)
				annotations["inter-router-port"] = "443"
				annotations["inter-router-host"] = string(current.NetworkSettings.IPAddress)

				certData := certs.GenerateCertificateData(subject, subject, string(current.NetworkSettings.IPAddress), caData)
				certs.PutCertificateData(subject, secretFile, certData, annotations)
			}
		} else {
			fmt.Println("Edge mode configuration cannot accept connections")
		}
	} else if dockerapi.IsErrNotFound(err) {
		fmt.Println("Router container does not exist (need init?): " + err.Error())
	} else {
		fmt.Println("Error retrieving router container: " + err.Error())
	}
}

func status(listConnectors bool) {
	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)

	current, err := dd.InspectContainer("skupper-router")
	if err == nil {
		mode := getRouterMode(current)
		var modedesc string
		if mode == RouterModeEdge {
			modedesc = " in edge mode"
		}
		fmt.Printf("Skupper status %s %s", current.State.Status, modedesc)
		fmt.Println()
		if listConnectors {
			fmt.Println("You want connectors do you")
		}
	} else if dockerapi.IsErrNotFound(err) {
		fmt.Println("Skupper is not currently enabled")
	} else {
		log.Fatal("Failed to retrieve router container: ", err.Error())
	}

}

func delete() {
	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)

	_, err := dd.InspectContainer("skupper-service-sync")
	if err == nil {
		dd.StopContainer("skupper-service-sync", 10*time.Second)
		if err != nil {
			log.Fatal("Failed to stop service sync container: ", err.Error())
		}
		err = dd.RemoveContainer("skupper-service-sync", dockertypes.ContainerRemoveOptions{})
		if err != nil {
			log.Fatal("Failed to remove service sync container: ", err.Error())
		}
	}

	filters := dockerfilters.NewArgs()
	filters.Add("label", "skupper.io/component")

	opts := dockertypes.ContainerListOptions{
		Filters: filters,
		All:     true,
	}
	containers, err := dd.ListContainers(opts)
	if err != nil {
		log.Fatal("Failed to list proxy containers: ", err.Error())
	}

	fmt.Println("Stopping skupper proxy containers...")
	for _, container := range containers {
		if value, ok := container.Labels["skupper.io/component"]; ok {
			if value == "proxy" {
				err := dd.StopContainer(container.ID, 10*time.Second)
				if err != nil {
					log.Fatal("Failed to stop proxy container: ", err.Error())
				}
				err = dd.RemoveContainer(container.ID, dockertypes.ContainerRemoveOptions{})
				if err != nil {
					log.Fatal("Failed to remove proxy container: ", err.Error())
				}
			}
		}
	}

	fmt.Println("Stopping skupper-router container...")
	err = dd.StopContainer("skupper-router", 10*time.Second)
	if err == nil {
		err := dd.RemoveContainer("skupper-router", dockertypes.ContainerRemoveOptions{})
		if err != nil {
			log.Fatal("Failed to remove router container: ", err.Error())
		}
	} else if dockerapi.IsErrNotFound(err) {
		fmt.Println("Router container does not exist")
	} else {
		log.Fatal("Failed to stop router container: ", err.Error())
	}

	fmt.Println("Removing skupper-network network...")
	// first remove any containers that remain on the network
	tnr, err := dd.InspectNetwork("skupper-network")
	if err == nil {
		for _, container := range tnr.Containers {
			err := dd.DisconnectContainerFromNetwork("skupper-network", container.Name, true)
			if err != nil {
				log.Fatal("Failed to disconnect container from skupper-network: ", err.Error())
			}
		}
	}
	err = dd.RemoveNetwork("skupper-network")
	if err != nil {
		log.Fatal("Failed to remove skupper-network network: ", err.Error())
	}

	log.Println("Removing files and directory...")
	err = os.RemoveAll(hostPath)
	if err != nil {
		log.Fatal("Failed to remove skupper files and directory: ", err.Error())
	}
	fmt.Println("Skupper resources now removed")
}

func connect(secretFile string, connectorName string, cost int) {
	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)
	// TODO: how to detect duplicate connection tokens?
	// examine host and port for each configured connector, should not collide

	secret := certs.GetSecretContent(secretFile)
	if secret != nil {
		existing, err := dd.InspectContainer("skupper-router")
		if err == nil {
			mode := getRouterMode(existing)

			if connectorName == "" {
				connectorName = generateConnectorName(skupperConnPath)
			}
			connPath := skupperConnPath + connectorName
			if err := os.Mkdir(connPath, 0755); err != nil {
				log.Fatal("Failed to create skupper connector directory: ", err.Error())
			}
			for k, v := range secret {
				if err := ioutil.WriteFile(connPath+"/"+k, v, 0755); err != nil {
					log.Fatal("Failed to write connector certificate file: ", err.Error())
				}
			}
			//read annotation files to get the host and port to connect to
			// TODO: error handling
			connector := Connector{
				Name: connectorName,
				Cost: cost,
			}
			if mode == RouterModeInterior {
				hostString, _ := ioutil.ReadFile(connPath + "/inter-router-host")
				portString, _ := ioutil.ReadFile(connPath + "/inter-router-port")
				connector.Host = string(hostString)
				connector.Port = string(portString)
				connector.Role = ConnectorRoleInterRouter
			} else {
				hostString, _ := ioutil.ReadFile(connPath + "/edge-host")
				portString, _ := ioutil.ReadFile(connPath + "/edge-port")
				connector.Host = string(hostString)
				connector.Port = string(portString)
				connector.Role = ConnectorRoleEdge
			}
			fmt.Printf("Skupper configured to connect to %s:%s (name=%s)", connector.Host, connector.Port, connector.Name)
			fmt.Println()
			restartRouterContainer(dd)
		} else {
			log.Fatal("Failed to retrieve router container (need init?): ", err.Error())
		}
	} else {
		log.Fatal("Failed to make connector, missing connection-token content")
	}
}

func disconnect(name string) {
	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)
	_, err := dd.InspectContainer("skupper-router")
	if err == nil {
		err = os.RemoveAll(skupperConnPath + name)
		restartRouterContainer(dd)
	} else {
		log.Fatal("Failed to retrieve router container (need init?): ", err.Error())
	}
}

func skupperInit(router *Router) {
	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)

	if router.Name == "" {
		info, err := dd.Info()
		if err != nil {
			log.Fatal("Failed to retrieve docker client info: ", err.Error())
		} else {
			router.Name = info.Name
		}
	}

	imageName := "quay.io/interconnectedcloud/qdrouterd"
	err := dd.PullImage(imageName, dockertypes.AuthConfig{}, dockertypes.ImagePullOptions{})
	if err != nil {
		log.Fatal("Failed to pull qdrouterd image: ", err.Error())
	}

	_, err = dd.CreateNetwork("skupper-network")
	if err != nil {
		log.Fatal("Failed to create skupper network: ", err.Error())
	}

	volumes := []string{"skupper", "skupper-amqps"}
	if router.Mode == RouterModeInterior {
		volumes = append(volumes, "skupper-internal")
	}

	createRouterHostFiles(volumes)

	opts := makeRouterContainerCreateConfig(router)
	_, err = dd.CreateContainer(*opts)
	if err != nil {
		log.Fatal("Failed to create skupper router container: ", err.Error())
	}

	err = dd.StartContainer(opts.Name)
	if err != nil {
		log.Fatal("Failed to start skupper router container: ", err.Error())
	}

	caData := ensureCA("skupper-ca")
	generateCredentials(caData, "skupper-amqps", opts.Name, opts.Name, false)
	generateCredentials(caData, "skupper", opts.Name, "", true)
	if router.Mode == RouterModeInterior {
		internalCaData := ensureCA("skupper-internal-ca")
		generateCredentials(internalCaData, "skupper-internal", "skupper-internal", "skupper-internal", false)
	}

	if router.Console == ConsoleAuthModeInternal {
		ensureSaslConfig()
		ensureSaslUsers(router.ConsoleUser, router.ConsolePassword)
	}

	startServiceSync(dd)
}

func restartProxies(dd libdocker.Interface) {

	filters := dockerfilters.NewArgs()
	filters.Add("label", "skupper.io/component")

	containers, err := dd.ListContainers(dockertypes.ContainerListOptions{
		Filters: filters,
		All:     true,
	})
	if err != nil {
		log.Fatal("Failed to list proxy containers: ", err.Error())
	}

	for _, container := range containers {
		labels := container.Labels
		for k, v := range labels {
			if k == "skupper.io/component" && v == "proxy" {

				err = dd.RestartContainer(container.ID, 10*time.Second)
				if err != nil {
					log.Fatal("Failed to restart proxy container: ", err.Error())
				}
			}
		}
	}

}

func startServiceSync(dd libdocker.Interface) {
	_, err := dd.InspectContainer("skupper-service-sync")
	if err == nil {
		fmt.Println("Container skupper-service-syncy already exists")
		return
	} else if dockerapi.IsErrNotFound(err) {
		var imageName string
		if os.Getenv("SERVICE_SYNC_IMAGE") != "" {
			imageName = os.Getenv("SERVICE_SYNC_IMAGE")
		} else {
			imageName = "quay.io/ajssmith/service-sync-go"
		}
		err := dd.PullImage(imageName, dockertypes.AuthConfig{}, dockertypes.ImagePullOptions{})
		if err != nil {
			log.Fatal("Failed to pull service sync image: ", err.Error())
		}

		containerCfg := &dockercontainer.Config{
			Hostname: "skupper-service-sync",
			Image:    imageName,
			Cmd:      []string{"app"},
		}
		hostCfg := &dockercontainer.HostConfig{
			Mounts: []dockermounttypes.Mount{
				{
					Type:   dockermounttypes.TypeBind,
					Source: skupperCertPath + "skupper",
					Target: "/etc/messaging",
				},
				{
					Type:   dockermounttypes.TypeBind,
					Source: "/var/run",
					Target: "/var/run",
				},
			},
			Privileged: true,
		}
		networkCfg := &dockernetworktypes.NetworkingConfig{
			EndpointsConfig: map[string]*dockernetworktypes.EndpointSettings{
				"skupper-network": {},
			},
		}
		opts := &dockertypes.ContainerCreateConfig{
			Name:             "skupper-service-sync",
			Config:           containerCfg,
			HostConfig:       hostCfg,
			NetworkingConfig: networkCfg,
		}
		_, err = dd.CreateContainer(*opts)
		if err != nil {
			log.Fatal("Failed to create service sync container: ", err.Error())
		}
		err = dd.StartContainer(opts.Name)
		if err != nil {
			log.Fatal("Failed to start service sync container: ", err.Error())
		}

	} else {
		log.Fatal("Failed to create skupper-service-sync container")
	}

}

func attachToVAN(bridge Bridge) bool {
	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)

	if bridge.Process != "" {
		if bridge.Name == bridge.Process {
			fmt.Println("The bridge and process names must be unique")
			return false
		}
		_, err := dd.InspectContainer(bridge.Process)
		if err != nil {
			if dockerapi.IsErrNotFound(err) {
				fmt.Println("The process to bridge was not found (need docker run?)")
				return false
			} else {
				log.Fatal("Failed to retrieve process container: ", err.Error())
			}
		}
	}

	_, err := dd.InspectContainer(bridge.Name)
	if err == nil {
		fmt.Printf("A Bridge with the address %s already exists, please choose a different one", bridge.Name)
		fmt.Println()
	} else if dockerapi.IsErrNotFound(err) {
		var imageName string
		if os.Getenv("PROXY_IMAGE") != "" {
			imageName = os.Getenv("PROXY_IMAGE")
		} else {
			imageName = "quay.io/ajssmith/icproxy-simple"
		}
		err := dd.PullImage(imageName, dockertypes.AuthConfig{}, dockertypes.ImagePullOptions{})
		if err != nil {
			log.Fatal("Failed to pull proxy image: ", err.Error())
		}

		labels := getLabels("proxy", bridge)
		bridges := []string{}
		bridges = append(bridges, "amqp:"+bridge.Name+"=>"+bridge.Protocol+":"+bridge.Port)
		if bridge.Process != "" {
			bridges = append(bridges, bridge.Protocol+":"+bridge.Port+"=>amqp:"+bridge.Name)
		}
		bridgeCfg := strings.Join(bridges, ",")

		containerCfg := &dockercontainer.Config{
			Hostname: bridge.Name,
			Image:    imageName,
			Cmd: []string{
				"node",
				"/opt/app-root/bin/simple.js",
				bridgeCfg},
			Env:    proxyEnv(bridge),
			Labels: labels,
		}
		hostCfg := &dockercontainer.HostConfig{
			Mounts: []dockermounttypes.Mount{
				{
					Type:   dockermounttypes.TypeBind,
					Source: skupperCertPath + "skupper",
					Target: "/etc/messaging",
				},
			},
			Privileged: true,
		}
		networkCfg := &dockernetworktypes.NetworkingConfig{
			EndpointsConfig: map[string]*dockernetworktypes.EndpointSettings{
				"skupper-network": {},
			},
		}

		opts := &dockertypes.ContainerCreateConfig{
			Name:             bridge.Name,
			Config:           containerCfg,
			HostConfig:       hostCfg,
			NetworkingConfig: networkCfg,
		}
		_, err = dd.CreateContainer(*opts)
		if err != nil {
			log.Fatal("Failed to create proxy container: ", err.Error())
		}
		err = dd.StartContainer(opts.Name)
		if err != nil {
			log.Fatal("Failed to start proxy container: ", err.Error())
		}
	} else {
		log.Fatal("Failed to create proxy container: ", err.Error())
	}

	err = dd.ConnectContainerToNetwork("skupper-network", bridge.Process)
	if err != nil {
		log.Fatal("Failed to connect bridge process to skupper-network: ", err.Error())
	}
	return true
}

func listServices() {
	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)

	filters := dockerfilters.NewArgs()
	filters.Add("label", "skupper.io/component")

	containers, err := dd.ListContainers(dockertypes.ContainerListOptions{
		Filters: filters,
		All:     true,
	})
	if err != nil {
		log.Fatal("Failed to list proxy containers: ", err.Error())
	}

	for _, container := range containers {
		for k, v := range container.Labels {
			if k == "skupper.io/service" {
				log.Printf("Service %s configuration: %s \n", container.Names[0], v)
			}
		}
	}
}

func requiredArg(name string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("%s must be specified", name)
		}
		if len(args) > 1 {
			return fmt.Errorf("illegal argument: %s", args[1])
		}
		return nil
	}
}

func main() {
	var skupperName string
	var isEdge bool
	var enableProxyController bool
	var enableServiceSync bool
	var enableRouterConsole bool
	var routerConsoleAuthMode string
	var routerConsoleUser string
	var routerConsolePassword string

	var cmdInit = &cobra.Command{
		Use:   "init",
		Short: "Initialize skupper docker installation",
		Long:  `init will setup a router and other supporting objects to provide a functional skupper installation that can be connected to a VAN deployment`,
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			router := Router{
				Name: skupperName,
				Mode: RouterModeInterior,
			}
			if isEdge {
				router.Mode = RouterModeEdge
			} else {
				router.Mode = RouterModeInterior
			}
			if enableRouterConsole {
				if routerConsoleAuthMode == string(ConsoleAuthModeInternal) || routerConsoleAuthMode == "" {
					router.Console = ConsoleAuthModeInternal
					router.ConsoleUser = routerConsoleUser
					router.ConsolePassword = routerConsolePassword
					if router.ConsoleUser == "" {
						router.ConsoleUser = "admin"
					}
					if router.ConsolePassword == "" {
						router.ConsolePassword = randomId(10)
					}
				} else {
					if routerConsoleUser != "" {
						log.Fatal("--router-console-user only valid when --router-console-auth=internal")
					}
					if routerConsolePassword != "" {
						log.Fatal("--router-console-password only valid when --router-console-auth=internal")
					}
					if routerConsoleAuthMode == string(ConsoleAuthModeUnsecured) {
						router.Console = ConsoleAuthModeUnsecured
					} else {
						log.Fatal("Unrecognised router console authentication mode: ", routerConsoleAuthMode)
					}
				}
			}
			skupperInit(&router)
			fmt.Println("Skupper is now installed.  Use 'skupper status' to get more information.")
		},
	}

	cmdInit.Flags().StringVarP(&skupperName, "id", "", "", "Provide a specific identity for the skupper installation")
	cmdInit.Flags().BoolVarP(&isEdge, "edge", "", false, "Configure as an edge")
	cmdInit.Flags().BoolVarP(&enableProxyController, "enable-proxy-controller", "", false, "Setup the proxy controller as well as the router")
	cmdInit.Flags().BoolVarP(&enableServiceSync, "enable-service-sync", "", true, "Configure proxy controller to particiapte in service sync (not relevant if --enable-proxy-controller is false)")
	cmdInit.Flags().BoolVarP(&enableRouterConsole, "enable-router-console", "", false, "Enable router console")
	cmdInit.Flags().StringVarP(&routerConsoleAuthMode, "router-console-auth", "", "", "Authentication mode for router console. One of: 'internal', 'unsecured'")
	cmdInit.Flags().StringVarP(&routerConsoleUser, "router-console-user", "", "", "Router console user. Valid only when --router-console-auth=internal")
	cmdInit.Flags().StringVarP(&routerConsolePassword, "router-console-password", "", "", "Router console user. Valid only when --router-console-auth=internal")

	var cmdDelete = &cobra.Command{
		Use:   "delete",
		Short: "Delete skupper installation",
		Long:  `delete will delete any skupper related objects`,
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			delete()
		},
	}

	var clientIdentity string
	var cmdConnectionToken = &cobra.Command{
		Use:   "connection-token <output-file>",
		Short: "Create a connection token file with which another skupper installation can connect to this one",
		Args:  requiredArg("output-file"),
		Run: func(cmd *cobra.Command, args []string) {
			generateConnectSecret(clientIdentity, args[0])
		},
	}
	cmdConnectionToken.Flags().StringVarP(&clientIdentity, "client-identity", "i", "skupper", "Provide a specific identity as which connecting skupper installation will be authenticated")

	var listConnectors bool
	var cmdStatus = &cobra.Command{
		Use:   "status",
		Short: "Report status of skupper installation",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			status(listConnectors)
		},
	}
	cmdStatus.Flags().BoolVarP(&listConnectors, "list-connectors", "", false, "List configured outgoing connections")

	var connectionName string
	var cost int
	var cmdConnect = &cobra.Command{
		Use:   "connect <connection-token-file>",
		Short: "Connect this skupper installation to that which issued the specified connectionToken",
		Args:  requiredArg("connection-token"),
		Run: func(cmd *cobra.Command, args []string) {
			connect(args[0], connectionName, cost)
		},
	}
	cmdConnect.Flags().StringVarP(&connectionName, "connection-name", "", "", "Provide a specific name for the connection (used when removing it with disconnect)")
	cmdConnect.Flags().IntVarP(&cost, "cost", "", 1, "Specify a cost for this connection.")

	var cmdDisconnect = &cobra.Command{
		Use:   "disconnect <name>",
		Short: "Remove specified connection",
		Args:  requiredArg("connection name"),
		Run: func(cmd *cobra.Command, args []string) {
			disconnect(args[0])
		},
	}

	var cmdVersion = &cobra.Command{
		Use:   "version",
		Short: "Report version of skupper cli and services",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("client version           %s\n", version)
		},
	}

	var bridgeName string
	var bridgeProtocol string
	var bridgePort string
	var bridgeProcess string
	var cmdBridge = &cobra.Command{
		Use:   "bridge",
		Short: "Bridge address and process (optionally) to the VAN installation",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			bridge := Bridge{
				Name:     bridgeName,
				Port:     bridgePort,
				Process:  bridgeProcess,
				Protocol: bridgeProtocol,
			}
			if attachToVAN(bridge) {
				fmt.Printf("%s bridged to application network", bridgeName)
				fmt.Println()
			}
		},
	}
	cmdBridge.Flags().StringVarP(&bridgeName, "bridge-name", "", "", "Provide a unique name for the VAN address (used when removing it with leave)")
	cmdBridge.Flags().StringVarP(&bridgeProtocol, "bridge-protocol", "", "", "Bridge protocol one of tcp, udp, http, http-2, amqp")
	cmdBridge.Flags().StringVarP(&bridgePort, "bridge-port", "", "", "A city in Connecticut")
	cmdBridge.Flags().StringVarP(&bridgeProcess, "bridge-process", "", "", "Process to bind to bridge")

	var cmdList = &cobra.Command{
		Use:   "list",
		Short: "Report list of skupper proxy services",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			listServices()
		},
	}

	var rootCmd = &cobra.Command{Use: "skupper"}
	rootCmd.Version = version
	rootCmd.AddCommand(cmdInit, cmdDelete, cmdConnectionToken, cmdConnect, cmdDisconnect, cmdStatus, cmdVersion, cmdBridge, cmdList)
	rootCmd.Execute()
}
