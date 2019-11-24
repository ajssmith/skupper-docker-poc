package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"golang.org/x/net/context"

	"github.com/spf13/cobra"
	//"github.com/ajssmith/skupper-docker/pkg/certs"
	//clicerts "github.com/skupperproject/skupper-cli/pkg/certs"
	"github.com/skupperproject/skupper-cli/pkg/certs"
)

var (
	version         = "v0.1"
	hostPath        = "/tmp/skupper"
	skupperCertPath = "/tmp/skupper/qpid-dispatch-certs/"
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
    "host": "skupper-messaging",
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
    certFile: /etc/qpid-dispatch-certs/{{.Name}}/tls.crt
    privateKeyFile: /etc/qpid-dispatch-certs/{{.Name}}/tls.key
    caCertFile: /etc/qpid-dispatch-certs/{{.Name}}/ca.crt
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

func findEnvVar(env []string, name string) *string {
	for _, v := range env {
		if strings.HasPrefix(v, name) {
			return &v
		}
	}
	return nil
}

func setEnvVar(cfg *container.Config, name string, value string) {
	original := cfg.Env
	updated := []string{}
	for _, v := range original {
		if strings.HasPrefix(v, name) {
			updated = append(updated, name+"="+value)
		} else {
			updated = append(updated, v)
		}
	}
	cfg.Env = updated
}

func isInterior(tcj types.ContainerJSON) bool {
	config := findEnvVar(tcj.Config.Env, "QDROUTERD_CONF")
	if config == nil {
		log.Fatal("Could not retrieve router config")
	}
	return true
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
		log.Fatal("Could not retrieve connection certs")
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
		envVars = append(envVars, "QDROUTERD_AUTO_MESH_DISCOVERY=QUERY")
	}
	envVars = append(envVars, "QDROUTERD_CONF="+routerConfig(router))

	if router.Console == ConsoleAuthModeInternal {
		envVars = append(envVars, "QDROUTERD_AUTO_CREATE_SASLDB_SOURCE=/etc/qpid-dispatch/sasl-users/")
		envVars = append(envVars, "QDROUTERD_AUTO_CREATE_SASLDB_PATH=/tmp/qdrouterd.sasldb")
	}

	return envVars
}

func getLabels(component string) map[string]string {
	//TODO: clarify use of labels vs k8s
	application := "skupper"
	if component == "router" {
		//the automeshing function of the router image expects the application
		//to be used as a unique label for identifying routers to connect to
		application = "skupper-router"
	}
	return map[string]string{
		"application":          application,
		"skupper.io/component": component,
		"prometheus.io/port":   "9090",
		"prometheus.io/scrape": "true",
	}
}

func skupperDir() string {
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		panic(err)
	}
	return dir
}

func randomId(length int) string {
	buffer := make([]byte, length)
	rand.Read(buffer)
	result := base64.StdEncoding.EncodeToString(buffer)
	return result[:length]
}

func getCertData(name string) certs.CertificateData {
	certData := certs.CertificateData{}
	// TODO: error handling
	certString, _ := ioutil.ReadFile(hostPath + "/qpid-dispatch-certs/" + name + "/tls.crt")
	keyString, _ := ioutil.ReadFile(hostPath + "/qpid-dispatch-certs/" + name + "/tls.key")
	caString, _ := ioutil.ReadFile(hostPath + "/qpid-dispatch-certs/" + name + "/ca.cert")
	certData["tls.crt"] = []byte(certString)
	certData["tls.key"] = []byte(keyString)
	certData["ca.cert"] = []byte(caString)
	return certData
}

func ensureCA(name string) certs.CertificateData {
	// check if existing by looking at path/dir, if not create dir to persist
	caData := certs.GenerateCACertificateData(name, name)
	if err := os.Mkdir(hostPath+"/qpid-dispatch-certs/"+name, 0755); err != nil {
		panic(err)
	}
	if err := ioutil.WriteFile(skupperCertPath+name+"/tls.crt", caData["tls.crt"], 0755); err != nil {
		panic(err)
	}
	if err := ioutil.WriteFile(skupperCertPath+name+"/tls.key", caData["tls.key"], 0755); err != nil {
		panic(err)
	}
	if err := ioutil.WriteFile(skupperCertPath+name+"/ca.cert", caData["ca.cert"], 0755); err != nil {
		panic(err)
	}
	return caData
}

func generateCredentials(caData certs.CertificateData, name string, subject string, hosts string, includeConnectJson bool) {
	certData := certs.GenerateCertificateData(name, subject, hosts, caData)
	if err := ioutil.WriteFile(skupperCertPath+name+"/tls.crt", certData["tls.crt"], 0755); err != nil {
		panic(err)
	}
	if err := ioutil.WriteFile(skupperCertPath+name+"/tls.key", certData["tls.key"], 0755); err != nil {
		panic(err)
	}
	if err := ioutil.WriteFile(skupperCertPath+name+"/ca.cert", certData["ca.cert"], 0755); err != nil {
		panic(err)
	}
	if includeConnectJson {
		certData["connect.json"] = []byte(connectJson())
		if err := ioutil.WriteFile(skupperCertPath+name+"/connect.json", []byte(connectJson()), 0755); err != nil {
			panic(err)
		}
	}
}

func RouterHostConfig(router *Router, volumes []string) *container.HostConfig {

	// only called during init, remove previous files and start anew
	_ = os.RemoveAll(hostPath)
	if err := os.MkdirAll(hostPath, 0755); err != nil {
		panic(err)
	}
	if err := os.Mkdir(hostPath+"/qpid-dispatch-certs/", 0755); err != nil {
		panic(err)
	}
	if err := os.Mkdir(hostPath+"/certs/", 0755); err != nil {
		panic(err)
	}
	for _, v := range volumes {
		if err := os.Mkdir(hostPath+"/qpid-dispatch-certs/"+v, 0755); err != nil {
			panic(err)
		}
	}

	hostcfg := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeBind,
				Source: hostPath + "/certs",
				Target: "/etc/qpid-dispatch/certs",
			},
			{
				Type:   mount.TypeBind,
				Source: hostPath + "/qpid-dispatch-certs",
				Target: "/etc/qpid-dispatch-certs",
			},
		},
		Privileged: true,
	}
	return hostcfg
}

func RouterContainer(router *Router) *container.Config {
	var image string

	labels := getLabels("router")

	if os.Getenv("QDROUTERD_IMAGE") != "" {
		image = os.Getenv("QDROUTERD_IMAGE")
	} else {
		image = "quay.io/interconnectedcloud/qdrouterd"
	}
	cfg := &container.Config{
		Image: image,
		Env:   routerEnv(router),
		Healthcheck: &container.HealthConfig{
			Test:        []string{"curl --fail -s http://localhost:9090/healthz || exit 1"},
			StartPeriod: time.Duration(60),
		},
		Labels:       labels,
		ExposedPorts: routerPorts(router),
	}
	//    if router.Console == ConsoleAuthModeInternal {
	//        mountSecretVolume("skupper-console-users", "/etc/qpid-dispatch/sasl-users/", cfg)
	//        mountConfigVolume("skupper-sasl-config", "/etc/sasl2", cfg)
	//    }
	return cfg
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

	cccb, err := dd.Cli.ContainerCreate(dd.Ctx,
		routerContainer,
		routerHostConfig,
		nil,
		"skupper-router")
	if err != nil {
		panic(err)
	}
	if err = dd.Cli.ContainerStart(dd.Ctx, cccb.ID, types.ContainerStartOptions{}); err != nil {
		panic(err)
	}
	existing, err = dd.Cli.ContainerInspect(dd.Ctx, "skupper-router")
	if err != nil {
		panic(err)
	}
	return existing

}

func initCommon(router *Router, volumes []string, dd *DockerDetails) types.ContainerJSON {
	if router.Name == "" {
		info, err := dd.Cli.Info(dd.Ctx)
		if err != nil {
			panic(err)
		} else {
			router.Name = info.Name
		}
	}
	tcj := ensureRouterContainer(router, volumes, dd)

	// start here and come up with version of generateSecret called generateCertificate
	caData := ensureCA("skupper-ca")
	generateCredentials(caData, "skupper-amqps", "skupper-messaging", "skupper-messaging", false)
	generateCredentials(caData, "skupper", "skupper-messaging", "", true)

	return tcj
}

func initEdge(router *Router, dd *DockerDetails) types.ContainerJSON {
	return initCommon(router, []string{"skupper", "skupper-amqps"}, dd)
}

func initInterior(router *Router, dd *DockerDetails) types.ContainerJSON {
	tcj := initCommon(router, []string{"skupper", "skupper-amqps", "skupper-internal"}, dd)
	internalCaData := ensureCA("skupper-internal-ca")
	// note, come back once it is understood how the network acess will work to generates hoss
	generateCredentials(internalCaData, "skupper-internal", "skupper-internal", "skupper-internal", false)

	return tcj
}

func generateConnectSecret(subject string, secretFile string, dd *DockerDetails) {
	// verify that the local deployment is interior
	current, err := dd.Cli.ContainerInspect(dd.Ctx, "skupper-router")
	if err == nil {
		if isInterior(current) {
			caData := getCertData("skupper-internal")
			fmt.Println("CA Data %s", caData["tls.crt"])
			fmt.Println("Interior mode configuration can accept connections")
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
		panic(err)
	}
	fmt.Println("Skupper current:", current.Name)
}

func deleteSkupper(dd *DockerDetails) {

	fmt.Print("Stopping container...")
	err := dd.Cli.ContainerStop(dd.Ctx, "skupper-router", nil)
	if err == nil {
		fmt.Print("Removing container...")
		if err := dd.Cli.ContainerRemove(dd.Ctx, "skupper-router", types.ContainerRemoveOptions{}); err != nil {
			panic(err)
		}
		fmt.Println("Skupper is now removed")
	} else if client.IsErrNotFound(err) {
		fmt.Println("Router container does not exist")
	} else {
		fmt.Println("Error deleting router container: " + err.Error())
	}
}

func addConnector(connector *Connector, tcj types.ContainerJSON) {
	// TODO: think about this, may need to generate a new config and stop, start container
	config := findEnvVar(tcj.Config.Env, "QDROUTERD_CONF")
	if config == nil {
		log.Fatal("Could not retrieve router config")
	}
	updated := *config + connectorConfig(connector)

	fmt.Println("Updated", updated)
}

func connect(secretFile string, isYaml bool, connectorName string, cost int, dd *DockerDetails) {
	if connectorName == "" {
		connectorName = generateConnectorName(skupperCertPath)
		fmt.Println("Connector name", connectorName)
	}
	connPath := skupperCertPath + connectorName
	if isYaml {
		// TODO: connector name to form path, check for already exists?
		if err := os.Mkdir(connPath, 0755); err != nil {
			panic(err)
		}
		certs.WriteSecretToFiles(secretFile, connPath)
	}
	// else we could keep secret file as a tar
	//determine if local deployment is edge or interior
	current, err := dd.Cli.ContainerInspect(dd.Ctx, "skupper-router")
	if err == nil {
		mode := getRouterMode(current)
		fmt.Println("Router mode: ", mode)

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
		addConnector(&connector, current)
	} else {
		fmt.Println("Failed to retrieve router container: ", err.Error())
	}
	// TODO, i think container has to get restarted, look at ctx.ClientUpdateConfig
	// it does not look like environment can be updated with this call
	// How do i ensure that I don't lose connectors previously created
	// need a restart interior, restart edge that works of the creds that exist

	//			_, err = kube.Standard.AppsV1().Deployments(kube.Namespace).Update(current)
	//			if err != nil {
	//				fmt.Println("Failed to update router deployment: ", err.Error())
	//			} else {
	//				fmt.Printf("Skupper configured to connect to %s:%s (name=%s)", connector.Host, connector.Port, connector.Name)
	//				fmt.Println()
	//			}
	//		} else if errors.IsAlreadyExists(err) {
	//			fmt.Println("A connector secret of that name already exist, please choose a different name")
	//		} else {
	//			fmt.Println("Failed to create connector secret: ", err.Error())
	//		}

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
		panic(err)
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
				panic(err)
			}
			defer out.Close()
			io.Copy(os.Stdout, out)

			var tcj types.ContainerJSON
			if !isEdge {
				tcj = initInterior(&router, dd)
			} else {
				router.Mode = RouterModeEdge
				tcj = initEdge(&router, dd)
			}

			fmt.Println(tcj.ID)
		},
	}

	cmdInit.Flags().StringVarP(&skupperName, "id", "", "", "Provide a specific identity for the skupper installation")
	cmdInit.Flags().BoolVarP(&isEdge, "edge", "", false, "Configure as an edge")
	cmdInit.Flags().BoolVarP(&enableProxyController, "enable-proxy-controller", "", true, "Setup the proxy controller as well as the router")
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
	var isYaml bool
	var cmdConnect = &cobra.Command{
		Use:   "connect <connection-token-file>",
		Short: "Connect this skupper installation to that which issued the specified connectionToken",
		Args:  requiredArg("connection-token"),
		Run: func(cmd *cobra.Command, args []string) {
			//connect(args[0], connectionName, cost, initKubeConfig(namespace, context))
			connect(args[0], isYaml, connectionName, cost, initDockerConfig())
		},
	}
	cmdConnect.Flags().StringVarP(&connectionName, "connection-name", "", "", "Provide a specific name for the connection (used when removing it with disconnect)")
	cmdConnect.Flags().IntVarP(&cost, "cost", "", 1, "Specify a cost for this connection.")
	cmdStatus.Flags().BoolVarP(&isYaml, "is-yaml", "", true, "Yaml file contains creds else tar file")

	var cmdVersion = &cobra.Command{
		Use:   "version",
		Short: "Report version of skupper cli and services",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("client version           %s\n", version)
		},
	}

	var rootCmd = &cobra.Command{Use: "skupper"}
	rootCmd.Version = version
	rootCmd.AddCommand(cmdInit, cmdDelete, cmdConnectionToken, cmdConnect, cmdStatus, cmdVersion)
	rootCmd.Execute()
}
