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

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"golang.org/x/net/context"

	"github.com/skupperproject/skupper-cli/pkg/certs"
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

func connectJson(tcj types.ContainerJSON) string {
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
	var buff bytes.Buffer
	cj := template.Must(template.New("cj").Parse(connect_json))
	cj.Execute(&buff, tcj)
	return buff.String()
}

type ConsoleAuthMode string

const (
	ConsoleAuthModeOpenshift ConsoleAuthMode = "openshift"
	ConsoleAuthModeInternal                  = "internal"
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
	Cli *client.Client
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
    saslMechanisms: EXTERNAL
    authenticatePeer: true
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

//func mountConfigVolume(name string, path string, cfg *container.Config) {
//    // define volume in container
//    volumes := cfg.Volumes
//}

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

func isInterior(tcj types.ContainerJSON) bool {
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

func getRouterMode(tcj types.ContainerJSON) RouterMode {
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

	if router.Console == ConsoleAuthModeInternal {
		envVars = append(envVars, "QDROUTERD_AUTO_CREATE_SASLDB_SOURCE=/etc/qpid-dispatch/sasl-users/")
		envVars = append(envVars, "QDROUTERD_AUTO_CREATE_SASLDB_PATH=/tmp/qdrouterd.sasldb")
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
	//TODO: clarify use of labels vs k8s
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

func generateCredentials(caData certs.CertificateData, name string, subject string, hosts string, includeConnectJson bool, tcj types.ContainerJSON) {
	certData := certs.GenerateCertificateData(name, subject, hosts, caData)

	for k, v := range certData {
		if err := ioutil.WriteFile(skupperCertPath+name+"/"+k, v, 0755); err != nil {
			log.Fatal("Failed to write certificate file: ", err.Error())
		}
	}

	if includeConnectJson {
		certData["connect.json"] = []byte(connectJson(tcj))
		if err := ioutil.WriteFile(skupperCertPath+name+"/connect.json", []byte(connectJson(tcj)), 0755); err != nil {
			log.Fatal("Failed to write connect file: ", err.Error())
		}
	}
}

func RouterHostConfig(router *Router, volumes []string) *container.HostConfig {

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

	hostcfg := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeBind,
				Source: skupperConnPath,
				Target: "/etc/qpid-dispatch/connections",
			},
			{
				Type:   mount.TypeBind,
				Source: skupperCertPath,
				Target: "/etc/qpid-dispatch-certs",
			},
			{
				Type:   mount.TypeBind,
				Source: skupperConsoleUsersPath,
				Target: "/etc/qpid-dispatch/sasl-users/",
			},
			{
				Type:   mount.TypeBind,
				Source: skupperSaslConfigPath,
				Target: "/etc/sasl2",
			},
		},
		Privileged: true,
	}
	return hostcfg
}

func RouterContainer(router *Router) *container.Config {
	var image string

	labels := getLabels("router", Bridge{})

	if os.Getenv("QDROUTERD_IMAGE") != "" {
		image = os.Getenv("QDROUTERD_IMAGE")
	} else {
		image = "quay.io/interconnectedcloud/qdrouterd"
	}
	cfg := &container.Config{
		Hostname: "skupper-router",
		Image:    image,
		Env:      routerEnv(router),
		Healthcheck: &container.HealthConfig{
			Test:        []string{"curl --fail -s http://localhost:9090/healthz || exit 1"},
			StartPeriod: time.Duration(60),
		},
		Labels:       labels,
		ExposedPorts: routerPorts(router),
	}

	// TODO: I think this would move to host config
	//    if router.Console == ConsoleAuthModeInternal {
	//        mountSecretVolume("skupper-console-users", "/etc/qpid-dispatch/sasl-users/", cfg)
	//        mountConfigVolume("skupper-sasl-config", "/etc/sasl2", cfg)
	//    }
	return cfg
}

func restartRouterContainer(dd *DockerDetails) types.ContainerJSON {
	current, err := dd.Cli.ContainerInspect(dd.Ctx, "skupper-router")
	if err != nil {
		log.Fatal("Failed to retrieve router container (need init?): ", err.Error())
	} else {
		mounts := []mount.Mount{}
		for _, v := range current.Mounts {
			mounts = append(mounts, mount.Mount{
				Type:   v.Type,
				Source: v.Source,
				Target: v.Destination,
			})
		}
		hostCfg := &container.HostConfig{
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

		containerCfg := &container.Config{
			Hostname: current.Config.Hostname,
			Image:    current.Config.Image,
			Healthcheck: &container.HealthConfig{
				Test:        []string{"curl --fail -s http://localhost:9090/healthz || exit 1"},
				StartPeriod: time.Duration(60),
			},
			Labels:       current.Config.Labels,
			ExposedPorts: current.Config.ExposedPorts,
			Env:          newEnv,
		}

		// c-ya-later
		err = dd.Cli.ContainerStop(dd.Ctx, "skupper-router", nil)
		if err == nil {
			if err := dd.Cli.ContainerRemove(dd.Ctx, "skupper-router", types.ContainerRemoveOptions{}); err != nil {
				log.Fatal("Failed to remove router container: ", err.Error())
			}
		}

		cccb, err := dd.Cli.ContainerCreate(dd.Ctx,
			containerCfg,
			hostCfg,
			nil,
			"skupper-router")
		if err != nil {
			log.Fatal("Failed to re-create router container: ", err.Error())
		}
		if err = dd.Cli.ContainerStart(dd.Ctx, cccb.ID, types.ContainerStartOptions{}); err != nil {
			log.Fatal("Failed to start router container: ", err.Error())
		}
		current, err = dd.Cli.ContainerInspect(dd.Ctx, "skupper-router")
		if err != nil {
			log.Fatal("Failed to retrieve router container: ", err.Error())
		}
		if err := dd.Cli.NetworkConnect(dd.Ctx, "skupper-network", "skupper-router", &network.EndpointSettings{}); err != nil {
			log.Fatal("Failed to connect skupper router to network: ", err.Error())
		}
	}
	return current
}

func ensureRouterContainer(router *Router, volumes []string, dd *DockerDetails) types.ContainerJSON {

	existing, err := dd.Cli.ContainerInspect(dd.Ctx, "skupper-router")
	if err == nil {
		// Container exists, do we need to update or restart it?
		fmt.Println("Router container already exists")
		return existing
	}

	routerContainer := RouterContainer(router)
	routerHostConfig := RouterHostConfig(router, volumes)
	networkConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			"skupper-network": {},
		},
	}

	cccb, err := dd.Cli.ContainerCreate(dd.Ctx,
		routerContainer,
		routerHostConfig,
		networkConfig,
		"skupper-router")
	if err != nil {
		log.Fatal("Failed to create router container: ", err.Error())
	}
	if err = dd.Cli.ContainerStart(dd.Ctx, cccb.ID, types.ContainerStartOptions{}); err != nil {
		log.Fatal("Failed to start router container: ", err.Error())
	}
	existing, err = dd.Cli.ContainerInspect(dd.Ctx, "skupper-router")
	if err != nil {
		log.Fatal("Failed to retrieve router container: ", err.Error())
	}
	return existing

}

func ensureSkupperNetwork(router *Router, dd *DockerDetails) {
	_, err := dd.Cli.NetworkInspect(dd.Ctx, "skupper-network", types.NetworkInspectOptions{})
	if err == nil {
		// Network exists, nothing to see here
		fmt.Println("Skupper docker network already exists")
		return
	} else {
		_, err = dd.Cli.NetworkCreate(dd.Ctx, "skupper-network", types.NetworkCreate{
			CheckDuplicate: true,
			Driver:         "bridge",
		})
		if err != nil {
			log.Fatal("Failed to create docker network: ", err.Error())
		}
	}
}

func initCommon(router *Router, volumes []string, dd *DockerDetails) types.ContainerJSON {
	if router.Name == "" {
		info, err := dd.Cli.Info(dd.Ctx)
		if err != nil {
			log.Fatal("Failed to retrieve context info: ", err.Error())
		} else {
			router.Name = info.Name
		}
	}
	ensureSkupperNetwork(router, dd)
	tcj := ensureRouterContainer(router, volumes, dd)

	// myhost a.k.a "skupper-messaging"
	//myhost := string(tcj.NetworkSettings.IPAddress)
	myhost := "skupper-router"

	// start here and come up with version of generateSecret called generateCertificate
	caData := ensureCA("skupper-ca")
	generateCredentials(caData, "skupper-amqps", myhost, myhost, false, tcj)
	generateCredentials(caData, "skupper", myhost, "", true, tcj)

	if router.Console == ConsoleAuthModeInternal {
		ensureSaslConfig()
		ensureSaslUsers(router.ConsoleUser, router.ConsolePassword)
	}

	return tcj
}

func initEdge(router *Router, dd *DockerDetails) types.ContainerJSON {
	return initCommon(router, []string{"skupper", "skupper-amqps"}, dd)
}

func initInterior(router *Router, dd *DockerDetails) types.ContainerJSON {
	tcj := initCommon(router, []string{"skupper", "skupper-amqps", "skupper-internal"}, dd)
	internalCaData := ensureCA("skupper-internal-ca")
	// note, come back once it is understood how the network acess will work to generates host
	generateCredentials(internalCaData, "skupper-internal", "skupper-internal", "skupper-internal", false, tcj)
	return tcj
}

func generateConnectSecret(subject string, secretFile string, dd *DockerDetails) {
	// verify that the local deployment is interior
	current, err := dd.Cli.ContainerInspect(dd.Ctx, "skupper-router")
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
	} else if client.IsErrNotFound(err) {
		fmt.Println("Router container does not exist (need init?): " + err.Error())
	} else {
		fmt.Println("Error retrieving router container: " + err.Error())
	}
}

func status(dd *DockerDetails, listConnectors bool) {
	fmt.Println("LIst Connectors", listConnectors)
	current, err := dd.Cli.ContainerInspect(dd.Ctx, "skupper-router")
	if err != nil {
		log.Fatal("Failed to retrieve router container: ", err.Error())
	}
	fmt.Println("Skupper current:", current.Name)
}

func deleteSkupper(dd *DockerDetails) {

	// TODO: delete proxy containers too and delete /tmp/skupper
	filters := filters.NewArgs()
	filters.Add("label", "skupper.io/component")

	fmt.Print("Stopping skupper proxy containers...")
	containers, err := dd.Cli.ContainerList(dd.Ctx, types.ContainerListOptions{
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
				_ = dd.Cli.ContainerStop(dd.Ctx, container.ID, nil)
				_ = dd.Cli.ContainerRemove(dd.Ctx, container.ID, types.ContainerRemoveOptions{})
			}
		}
	}

	fmt.Print("Stopping skupper-router container...")

	err = dd.Cli.ContainerStop(dd.Ctx, "skupper-router", nil)
	if err == nil {
		fmt.Print("Removing container...")
		if err := dd.Cli.ContainerRemove(dd.Ctx, "skupper-router", types.ContainerRemoveOptions{}); err != nil {
			log.Fatal("Failed to remove router container: ", err.Error())
		}
		fmt.Println("Skupper is now removed")

	} else if client.IsErrNotFound(err) {
		fmt.Println("Router container does not exist")
	} else {
		log.Fatal("Failed to stop router container: ", err.Error())
	}
	// remove any networks that we created, first remove any containers
	// still on the network
	tnr, err := dd.Cli.NetworkInspect(dd.Ctx, "skupper-network", types.NetworkInspectOptions{})
	if err == nil {
		for _, container := range tnr.Containers {
			err := dd.Cli.NetworkDisconnect(dd.Ctx, "skupper-network", container.Name, true)
			if err != nil {
				log.Fatal("Failed to disconnect container from skupper-network: ", err.Error())
			}
		}
	}
	dd.Cli.NetworkRemove(dd.Ctx, "skupper-network")

	// remove /tmp/skupper
	err = os.RemoveAll(hostPath)
}

func connect(secretFile string, connectorName string, cost int, dd *DockerDetails) {
	// TODO: how to detect duplicate connection tokens?
	// examine host and port for each configured connector, should not collide

	secret := certs.GetSecretContent(secretFile)
	if secret != nil {
		existing, err := dd.Cli.ContainerInspect(dd.Ctx, "skupper-router")
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

func disconnect(name string, dd *DockerDetails) {
	_, err := dd.Cli.ContainerInspect(dd.Ctx, "skupper-router")
	if err == nil {
		// do we care if name directory does not exist?
		err = os.RemoveAll(skupperConnPath + name)
		restartRouterContainer(dd)
	} else {
		log.Fatal("Failed to retrieve router container (need init?): ", err.Error())
	}
}
func ensureProxy(bridge Bridge, dd *DockerDetails) {
	_, err := dd.Cli.ContainerInspect(dd.Ctx, bridge.Name+"-proxy")
	if err == nil {
		fmt.Println("A proxy bridge of that name already exists, please choose a different name")
	} else if client.IsErrNotFound(err) {
		var imageName string
		if os.Getenv("PROXY_IMAGE") != "" {
			imageName = os.Getenv("PROXY_IMAGE")
		} else {
			imageName = "quay.io/ajssmith/icproxy-simple"
		}
		out, err := dd.Cli.ImagePull(dd.Ctx, imageName, types.ImagePullOptions{})
		if err != nil {
			log.Fatal("Failed to pull proxy image: ", err.Error())
		}
		defer out.Close()

		labels := getLabels("proxy", bridge)

		bridges := []string{}

		bridges = append(bridges, "amqp:"+bridge.Name+"=>"+bridge.Protocol+":"+bridge.Port)
		bridges = append(bridges, bridge.Protocol+":"+bridge.Port+"=>amqp:"+bridge.Name)

		bridgeCfg := strings.Join(bridges, ",")

		containerCfg := &container.Config{
			Image: imageName,
			Cmd: []string{
				"node",
				"/opt/app-root/bin/simple.js",
				bridgeCfg},
			Env:    proxyEnv(bridge),
			Labels: labels,
		}
		hostCfg := &container.HostConfig{
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeBind,
					Source: hostPath + "/qpid-dispatch-certs/skupper",
					Target: "/etc/messaging",
				},
			},
			Privileged: true,
		}
		cccb, err := dd.Cli.ContainerCreate(dd.Ctx,
			containerCfg,
			hostCfg,
			nil,
			bridge.Name+"-proxy")
		if err != nil {
			log.Fatal("Failed to create proxy bridge container: ", err.Error())
		}
		if err = dd.Cli.ContainerStart(dd.Ctx, cccb.ID, types.ContainerStartOptions{}); err != nil {
			log.Fatal("Failed to start proxy bridge container: ", err.Error())
		}
		_, err = dd.Cli.ContainerInspect(dd.Ctx, bridge.Name)
		if err != nil {
			log.Fatal("Failed to retrieve proxy bridge container: ", err.Error())
		}
	} else {
		log.Fatal("Failed to ensure proxy bridge container: ", err.Error())
	}
}

func attachToVAN(bridge Bridge, dd *DockerDetails) bool {

	if bridge.Process != "" {
		if bridge.Name == bridge.Process {
			fmt.Println("The bridge and process names must be unique")
			return false
		}
		_, err := dd.Cli.ContainerInspect(dd.Ctx, bridge.Process)
		if err != nil {
			if client.IsErrNotFound(err) {
				fmt.Println("The bridge process was not found (need docker run?)")
				return false
			} else {
				log.Fatal("Failed to retrieve process container: ", err.Error())
			}
		}
	}

	_, err := dd.Cli.ContainerInspect(dd.Ctx, bridge.Name)
	if err == nil {
		fmt.Printf("A Bridge with the address %s already exists, please choose a different one", bridge.Name)
		fmt.Println()
	} else if client.IsErrNotFound(err) {
		var imageName string
		if os.Getenv("PROXY_IMAGE") != "" {
			imageName = os.Getenv("PROXY_IMAGE")
		} else {
			imageName = "quay.io/ajssmith/icproxy-simple"
		}
		out, err := dd.Cli.ImagePull(dd.Ctx, imageName, types.ImagePullOptions{})
		if err != nil {
			log.Fatal("Failed to pull proxy image: ", err.Error())
		}
		defer out.Close()

		labels := getLabels("proxy", bridge)

		bridges := []string{}

		bridges = append(bridges, "amqp:"+bridge.Name+"=>"+bridge.Protocol+":"+bridge.Port)
		if bridge.Process != "" {
			bridges = append(bridges, bridge.Protocol+":"+bridge.Port+"=>amqp:"+bridge.Name)
		}

		bridgeCfg := strings.Join(bridges, ",")

		containerCfg := &container.Config{
			Hostname: bridge.Name,
			Image:    imageName,
			Cmd: []string{
				"node",
				"/opt/app-root/bin/simple.js",
				bridgeCfg},
			Env:    proxyEnv(bridge),
			Labels: labels,
		}
		hostCfg := &container.HostConfig{
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeBind,
					Source: skupperCertPath + "skupper",
					Target: "/etc/messaging",
				},
			},
			Privileged: true,
		}
		networkCfg := &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				"skupper-network": {},
			},
		}
		cccb, err := dd.Cli.ContainerCreate(dd.Ctx,
			containerCfg,
			hostCfg,
			networkCfg,
			bridge.Name)
		if err != nil {
			log.Fatal("Failed to create proxy container: ", err.Error())
		}
		if err = dd.Cli.ContainerStart(dd.Ctx, cccb.ID, types.ContainerStartOptions{}); err != nil {
			log.Fatal("Failed to start proxy container: ", err.Error())
		}
		_, err = dd.Cli.ContainerInspect(dd.Ctx, bridge.Name)
		if err != nil {
			log.Fatal("Failed to retrieve proxy container: ", err.Error())
		}
	} else {
		log.Fatal("Failed to ensure proxy container: ", err.Error())
	}

	err = dd.Cli.NetworkConnect(dd.Ctx, "skupper-network", bridge.Process, &network.EndpointSettings{})
	if err != nil {
		// what if someone connected during docker run??
		log.Fatal("Failed to connect bridge process to skupper-network", err.Error())
	}
	return true
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

func initDockerConfig() *DockerDetails {
	dd := DockerDetails{}

	dd.Ctx = context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatal("Failed to create docker client: ", err.Error())
	} else {
		dd.Cli = cli
	}
	return &dd
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
			dd := initDockerConfig()
			imageName := "quay.io/interconnectedcloud/qdrouterd"

			out, err := dd.Cli.ImagePull(dd.Ctx, imageName, types.ImagePullOptions{})
			if err != nil {
				log.Fatal("Failed to pull qdrouterd image: ", err.Error())
			}
			defer out.Close()

			if !isEdge {
				_ = initInterior(&router, dd)
			} else {
				router.Mode = RouterModeEdge
				_ = initEdge(&router, dd)
			}

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
			deleteSkupper(initDockerConfig())
		},
	}

	var clientIdentity string
	var cmdConnectionToken = &cobra.Command{
		Use:   "connection-token <output-file>",
		Short: "Create a connection token file with which another skupper installation can connect to this one",
		Args:  requiredArg("output-file"),
		Run: func(cmd *cobra.Command, args []string) {
			generateConnectSecret(clientIdentity, args[0], initDockerConfig())
		},
	}
	cmdConnectionToken.Flags().StringVarP(&clientIdentity, "client-identity", "i", "skupper", "Provide a specific identity as which connecting skupper installation will be authenticated")

	var listConnectors bool
	var cmdStatus = &cobra.Command{
		Use:   "status",
		Short: "Report status of skupper installation",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			status(initDockerConfig(), listConnectors)
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
			connect(args[0], connectionName, cost, initDockerConfig())
		},
	}
	cmdConnect.Flags().StringVarP(&connectionName, "connection-name", "", "", "Provide a specific name for the connection (used when removing it with disconnect)")
	cmdConnect.Flags().IntVarP(&cost, "cost", "", 1, "Specify a cost for this connection.")

	var cmdDisconnect = &cobra.Command{
		Use:   "disconnect <name>",
		Short: "Remove specified connection",
		Args:  requiredArg("connection name"),
		Run: func(cmd *cobra.Command, args []string) {
			disconnect(args[0], initDockerConfig())
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
			dd := initDockerConfig()
			if attachToVAN(bridge, dd) {
				fmt.Printf("%s bridged to application network", bridgeName)
				fmt.Println()
			}
		},
	}
	cmdBridge.Flags().StringVarP(&bridgeName, "bridge-name", "", "", "Provide a unique name for the VAN address (used when removing it with leave)")
	cmdBridge.Flags().StringVarP(&bridgeProtocol, "bridge-protocol", "", "", "Bridge protocol one of tcp, udp, http, http-2, amqp")
	cmdBridge.Flags().StringVarP(&bridgePort, "bridge-port", "", "", "A city in Connecticut")
	cmdBridge.Flags().StringVarP(&bridgeProcess, "bridge-process", "", "", "Process to bind to bridge")

	var rootCmd = &cobra.Command{Use: "skupper"}
	rootCmd.Version = version
	rootCmd.AddCommand(cmdInit, cmdDelete, cmdConnectionToken, cmdConnect, cmdDisconnect, cmdStatus, cmdVersion, cmdBridge)
	rootCmd.Execute()
}
