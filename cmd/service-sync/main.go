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
	hostPath        = "/tmp/skupper"
	skupperCertPath = hostPath + "/qpid-dispatch-certs/"
)

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

func getLabels(origin string, service skupperservice.Service) map[string]string {
	serviceLabel, _ := json.Marshal(service)

	return map[string]string{
		"application":          "skupper-proxy",
		"skupper.io/address":   service.Name,
		"skupper.io/component": "proxy",
		"skupper.io/service":   string(serviceLabel),
		"skupper.io/origin":    origin,
	}
}

func reconcile_service(origin string, service map[string]interface{}) {
	var update = false
	var s skupperservice.Service
	var p skupperservice.ServicePort

	s.Name = service["name"].(string)
	s.Proxy = service["proxy"].(string)
	s.Process = ""

	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)

	for _, servicePort := range service["ports"].([]interface{}) {
		if servicePort, ok := servicePort.(map[string]interface{}); ok {
			p.Port = int32(servicePort["port"].(uint32))
			p.TargetPort = int32(servicePort["targetPort"].(uint32))
			p.Protocol = servicePort["protocol"].(string)
			s.Ports = append(s.Ports, p)
		}
	}

	current, err := dd.InspectContainer(s.Name)
	if err == nil {
		if label, ok := current.Config.Labels["skupper.io/service"]; ok {
			currentLabel, _ := json.Marshal(s)
			if label != string(currentLabel) {
				// delete then update
				fmt.Printf("Detected proxy config update for %s: %s\n", s.Name, label)
				undeploy(s.Name, dd)
				update = true
			}
		} // else why doesn't container have label?
	} else {
		update = true
	}

	if update {
		deploy(origin, s, dd)
	}
}

func reconcile(origin string, updates []interface{}) {
	for _, service := range updates {
		if service, ok := service.(map[string]interface{}); ok {
			reconcile_service(origin, service)
		}
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

	labels := getLabels(origin, service)
	bridges := []string{}

	bridges = append(bridges, string(service.Proxy)+":"+strconv.Itoa(int(service.Ports[0].Port))+"=>amqp:"+service.Name)
	if service.Process != "" && service.Process != "none" {
		bridges = append(bridges, "amqp:"+service.Name+"=>"+string(service.Proxy)+":"+strconv.Itoa(int(service.Ports[0].Port)))
	}
	bridgeCfg := strings.Join(bridges, ",")

	containerCfg := &dockercontainer.Config{
		Hostname: service.Name,
		Image:    imageName,
		Cmd: []string{
			"node",
			"/opt/app-root/bin/simple.js",
			bridgeCfg},
		Env:    []string{"ICPROXY_BRIDGE_HOST=" + service.Name},
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
		Name:             service.Name,
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

func syncSender(origin string, s *amqp.Session, sendLocal chan bool) {
	var request amqp.Message
	var properties amqp.MessageProperties

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

	for {
		select {
		case <-sendLocal:
			properties.Subject = "service-sync-update"
			ls := getLocalServices()
			for _, item := range ls {
				fmt.Println("Local service: ", item)
			}
			//err = sender.Send(ctx, &request)
		case <-ticker.C:
			properties.Subject = "service-sync-request"
			request.Value = ""
			err = sender.Send(ctx, &request)
			log.Println("Skupper service request sent")
		}
	}

}

func addLocalService(origin string, name string) {
	var svc skupperservice.Service

	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)
	file, err := ioutil.ReadFile(name)
	if err != nil {
		fmt.Println("Local service file could not be read", err.Error())
	} else {
		err := json.Unmarshal(file, &svc)
		if err != nil {
			fmt.Println("Error decoding services file", err.Error())
		}
		deploy(origin, svc, dd)
	}
}

func watchForLocal(origin string) {
	var watcher *fsnotify.Watcher

	watcher, _ = fsnotify.NewWatcher()
	defer watcher.Close()

	err := watcher.Add("/etc/messaging/services")
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
				addLocalService(origin, event.Name)
			} else if event.Op&fsnotify.Remove == fsnotify.Remove {
				log.Println("Sync local removed file: ", event.Name)
			} else {
				return
			}
		}
	}
}

func main() {
	log.Printf("Skupper service sync starting")

	localOrigin := os.Getenv("SKUPPER_SERVICE_SYNC_ORIGIN")

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

	go syncSender(localOrigin, session, sendLocal)
	go watchForLocal(localOrigin)

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
					if updates, ok := msg.Value.([]interface{}); ok {
						reconcile(updateOrigin, updates)
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
