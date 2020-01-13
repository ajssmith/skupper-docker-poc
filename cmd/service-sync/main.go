package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"pack.ag/amqp"
	"strconv"
	"strings"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	dockermounttypes "github.com/docker/docker/api/types/mount"
	dockernetworktypes "github.com/docker/docker/api/types/network"

	"github.com/ajssmith/skupper-docker/pkg/dockershim/libdocker"
	skupperservice "github.com/ajssmith/skupper-docker/pkg/service"
	"github.com/fsnotify/fsnotify"
)

const (
	ServiceSyncAddress = "mc/$skupper-service-sync"
)

var (
	hostPath            = "/tmp/skupper"
	skupperCertPath     = hostPath + "/qpid-dispatch-certs/"
	skupperServicesPath = "/etc/messaging/services/"
)

func describe(i interface{}) {
	fmt.Printf("(%v, %T)\n", i, i)
	fmt.Println()
}

func getTlsConfig(verify bool, cert, key, ca string) (*tls.Config, error) {
	var config tls.Config
	config.InsecureSkipVerify = true
	if verify {
		certPool := x509.NewCertPool()
		file, err := ioutil.ReadFile(ca)
		if err != nil {
			return nil, err
		}
		certPool.AppendCertsFromPEM(file)
		config.RootCAs = certPool
		config.InsecureSkipVerify = false
	}

	_, errCert := os.Stat(cert)
	_, errKey := os.Stat(key)
	if errCert == nil || errKey == nil {
		tlsCert, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			log.Fatal("Could not load x509 key pair", err.Error())
		}
		config.Certificates = []tls.Certificate{tlsCert}
	}
	config.MinVersion = tls.VersionTLS10

	return &config, nil
}

func authOption(username string, password string) amqp.ConnOption {
	if len(password) > 0 && len(username) > 0 {
		return amqp.ConnSASLPlain(username, password)
	} else {
		return amqp.ConnSASLAnonymous()
	}
}

func getLabels(origin string, service skupperservice.Service, isLocal bool) map[string]string {
	target := ""
	if isLocal {
		target = service.Targets[0].Name
	}
	return map[string]string{
		"application":          "skupper-proxy",
		"skupper.io/address":   service.Address,
		"skupper.io/target":    target,
		"skupper.io/component": "proxy",
		"skupper.io/origin":    origin,
	}
}

func reconcile_service(origin string, service skupperservice.Service) {
	var update = true

	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)

	_, err := dd.InspectContainer(service.Address)
	if err == nil {
		return
	}

	if update {
		deploy(origin, service, dd)
	}
}

func reconcile(origin string, svcDefs []skupperservice.Service) {
	for _, service := range svcDefs {
		fmt.Println("Reconcile service: ", service)
		reconcile_service(origin, service)
	}
}

func undeploy(name string, dd libdocker.Interface) {

	err := dd.StopContainer(name, 10*time.Second)
	if err == nil {
		if err := dd.RemoveContainer(name, dockertypes.ContainerRemoveOptions{}); err != nil {
			log.Fatal("Failed to remove proxy container: ", err.Error())
		}
	}
}

func deploy(origin string, service skupperservice.Service, dd libdocker.Interface) {
	//TODO: checking origing can drive local versus remote, e.g. remote has no target
	isLocal := true

	myOrigin := os.Getenv("SKUPPER_SERVICE_SYNC_ORIGIN")
	if origin != myOrigin {
		isLocal = false
	}

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

	// attach local target to the skupper network
	if isLocal {
		//	if service.Targets[0].Name != "" {
		err := dd.ConnectContainerToNetwork("skupper-network", service.Targets[0].Name)
		if err != nil {
			log.Fatal("Failed to attach target container to skupper network: ", err.Error())
		}
	}

	labels := getLabels(origin, service, isLocal)
	bridges := []string{}
	env := []string{}

	bridges = append(bridges, service.Protocol+":"+strconv.Itoa(int(service.Port))+"=>amqp:"+service.Address)
	if isLocal {
		//	if service.Targets[0].Name != "" {
		bridges = append(bridges, "amqp:"+service.Address+"=>"+service.Protocol+":"+strconv.Itoa(int(service.Targets[0].TargetPort)))
		env = append(env, "ICPROXY_BRIDGE_HOST="+service.Targets[0].Name)
	}
	bridgeCfg := strings.Join(bridges, ",")

	//Env:    []string{"ICPROXY_BRIDGE_HOST=" + service.Targets[0].Name},
	containerCfg := &dockercontainer.Config{
		Hostname: service.Address,
		Image:    imageName,
		Cmd: []string{
			"node",
			"/opt/app-root/bin/simple.js",
			bridgeCfg},
		Env:    env,
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
		Name:             service.Address,
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

}

func getLocalServices() []skupperservice.Service {
	items := []skupperservice.Service{}

	files, err := ioutil.ReadDir("/etc/messaging/services")
	if err == nil {
		for _, file := range files {
			svc := skupperservice.Service{}
			data, err := ioutil.ReadFile("/etc/messaging/services/" + file.Name())
			if err == nil {
				err = json.Unmarshal(data, &svc)
				if err == nil {
					items = append(items, svc)
				} else {
					fmt.Println("Error unmarshalling local services file: ", err.Error())
				}
			} else {
				fmt.Println("Error reading local services file: ", err.Error())
			}
		}
	} else {
		fmt.Println("Error reading local services directory", err.Error())
	}
	return items
}

func syncSender(s *amqp.Session, sendLocal chan bool) {
	var request amqp.Message
	var properties amqp.MessageProperties

	myOrigin := os.Getenv("SKUPPER_SERVICE_SYNC_ORIGIN")

	ctx := context.Background()
	sender, err := s.NewSender(
		amqp.LinkTargetAddress(ServiceSyncAddress),
	)
	if err != nil {
		log.Fatal("Failed to create sender:", err)
	}

	defer func() {
		sender.Close(ctx)
	}()

	ticker := time.NewTicker(10 * time.Second)

	properties.Subject = "service-sync-request"
	request.Properties = &properties
	request.ApplicationProperties = make(map[string]interface{})
	request.ApplicationProperties["origin"] = myOrigin

	for {
		select {
		case <-sendLocal:
			properties.Subject = "service-sync-update"
			svcDefs := getLocalServices()
			encoded, err := json.Marshal(svcDefs)
			if err != nil {
				fmt.Println("Failed to create json for service definition sync: ", err.Error())
				return
			} else {
				fmt.Println("Local services: ", string(encoded))
			}
			request.Value = string(encoded)
			//request.ApplicationProperties = make(map[string]interface{})
			//request.ApplicationProperties["origin"] = myOrigin
			//request.ApplicationProperties = map[string]interface{
			//    "origin": interface(origin),
			//}
			err = sender.Send(ctx, &request)
		case <-ticker.C:
			properties.Subject = "service-sync-request"
			request.Value = ""
			err = sender.Send(ctx, &request)
			log.Println("Skupper service request sent")
		}
	}

}

func deployLocalService(name string) {
	var svc skupperservice.Service

	origin := os.Getenv("SKUPPER_SERVICE_SYNC_ORIGIN")

	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)
	file, err := ioutil.ReadFile(name)
	if err != nil {
		fmt.Println("Local service file could not be read", err.Error())
	} else {
		err := json.Unmarshal(file, &svc)
		if err != nil {
			fmt.Println("Error decoding services file", err.Error())
			return
		}
		deploy(origin, svc, dd)
	}
}

func undeployLocalService(address string) {
	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)

	// file is removed so we have to inspect the container to get the label
	// of the target container that we need to disconnect from skupper network
	existing, err := dd.InspectContainer(address)
	if err != nil {
		fmt.Println("Local service container could not be retrieved", err.Error())
	} else {
		if target, ok := existing.Config.Labels["skupper.io/target"]; ok {
			if target != "" {
				err := dd.DisconnectContainerFromNetwork("skupper-network", target, true)
				if err != nil {
					log.Fatal("Failed to detatch target container from skupper network: ", err.Error())
				}
			}
		}
		undeploy(address, dd)
	}
}

func watchForLocal(path string) {
	var watcher *fsnotify.Watcher

	watcher, _ = fsnotify.NewWatcher()
	defer watcher.Close()

	err := watcher.Add(path)
	if err != nil {
		log.Fatal("Could not add local services directory watcher", err.Error())
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				log.Println("Sync local new file: ", event.Name)
			} else if event.Op&fsnotify.Write == fsnotify.Write {
				log.Println("Sync local modified file: ", event.Name)
				deployLocalService(event.Name)
			} else if event.Op&fsnotify.Remove == fsnotify.Remove {
				log.Println("Sync local removed file: ", event.Name)
				address := strings.TrimPrefix(event.Name, "/etc/messaging/services/")
				undeployLocalService(address)
			} else {
				return
			}
		}
	}
}

func main() {
	localOrigin := os.Getenv("SKUPPER_SERVICE_SYNC_ORIGIN")

	log.Println("Skupper service sync starting, local origin => ", localOrigin)

	config, err := getTlsConfig(true, "/etc/messaging/tls.crt", "/etc/messaging/tls.key", "/etc/messaging/ca.crt")

	client, err := amqp.Dial("amqps://skupper-router:5671", amqp.ConnSASLAnonymous(), amqp.ConnMaxFrameSize(4294967295), amqp.ConnTLSConfig(config))
	if err != nil {
		log.Fatal("Failed to connect to url", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		log.Fatal("Failed to create session:", err)
	}

	sendLocal := make(chan bool)

	go syncSender(session, sendLocal)
	go watchForLocal(skupperServicesPath)

	ctx := context.Background()
	receiver, err := session.NewReceiver(
		amqp.LinkSourceAddress(ServiceSyncAddress),
		amqp.LinkCredit(10),
	)
	if err != nil {
		log.Fatal("Failed to create receiver:", err.Error())
	}
	defer func() {
		ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
		receiver.Close(ctx)
		cancel()
	}()

	for {
		msg, err := receiver.Receive(ctx)
		if err != nil {
			log.Fatal("Reading message from service synch: ", err.Error())
		}
		// Decode message as it is either a request to send update
		// or it is a receipt that needs to be reconciled
		msg.Accept()
		subject := msg.Properties.Subject

		if subject == "service-sync-request" {
			sendLocal <- true
		} else if subject == "service-sync-update" {
			if updateOrigin, ok := msg.ApplicationProperties["origin"].(string); ok {
				if updateOrigin != localOrigin {
					if updates, ok := msg.Value.(string); ok {
						svcDefs := []skupperservice.Service{}
						err := json.Unmarshal([]byte(updates), &svcDefs)
						if err == nil {
							reconcile(updateOrigin, svcDefs)
						} else {
							fmt.Println("Error marshall sawyer", err.Error())
						}
					}
				}
			} else {
				log.Println("Skupper service sync update type assertion error")
			}
		} else {
			log.Println("Service sync subject not valid")
		}
	}
	log.Println("Skupper service sync terminating")

}
