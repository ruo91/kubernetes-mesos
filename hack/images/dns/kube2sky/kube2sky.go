/*
Copyright 2014 Google Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// kube2sky is a bridge between Kubernetes and SkyDNS.  It watches the
// Kubernetes master for changes in Services and manifests them into etcd for
// SkyDNS to serve as DNS records.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	kapi "github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	kclient "github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	kfields "github.com/GoogleCloudPlatform/kubernetes/pkg/fields"
	klabels "github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	tools "github.com/GoogleCloudPlatform/kubernetes/pkg/tools"
	kwatch "github.com/GoogleCloudPlatform/kubernetes/pkg/watch"
	etcd "github.com/coreos/go-etcd/etcd"
	skymsg "github.com/skynetservices/skydns/msg"
)

var (
	domain                = flag.String("domain", "kubernetes.local", "domain under which to create names")
	etcd_mutation_timeout = flag.Duration("etcd_mutation_timeout", 10*time.Second, "crash after retrying etcd mutation for a specified duration")
	etcd_server           = flag.String("etcd-server", "http://127.0.0.1:4001", "URL to etcd server")
	verbose               = flag.Bool("verbose", false, "log extra information")
)

func removeDNS(record string, etcdClient *etcd.Client) error {
	log.Printf("Removing %s from DNS", record)
	_, err := etcdClient.Delete(skymsg.Path(record), true)
	return err
}

func addDNS(record string, service *kapi.Service, etcdClient *etcd.Client) error {
	// if PortalIP is not set, a DNS entry should not be created
	if !kapi.IsServiceIPSet(service) {
		log.Printf("Skipping dns record for headless service: %s\n", service.Name)
		return nil
	}

	svc := skymsg.Service{
		Host:     service.Spec.PortalIP,
		Port:     service.Spec.Port,
		Priority: 10,
		Weight:   10,
		Ttl:      30,
	}
	b, err := json.Marshal(svc)
	if err != nil {
		return err
	}
	// Set with no TTL, and hope that kubernetes events are accurate.

	log.Printf("Setting dns record: %v -> %s:%d\n", record, service.Spec.PortalIP, service.Spec.Port)
	_, err = etcdClient.Set(skymsg.Path(record), string(b), uint64(0))
	return err
}

// Implements retry logic for arbitrary mutator. Crashes after retrying for
// etcd_mutation_timeout.
func mutateEtcdOrDie(mutator func() error) {
	timeout := time.After(*etcd_mutation_timeout)
	for {
		select {
		case <-timeout:
			log.Fatalf("Failed to mutate etcd for %v using mutator: %v", *etcd_mutation_timeout, mutator)
		default:
			if err := mutator(); err != nil {
				delay := 50 * time.Millisecond
				log.Printf("Failed to mutate etcd using mutator: %v due to: %v. Will retry in: %v", mutator, err, delay)
				time.Sleep(delay)
			} else {
				return
			}
		}
	}
}

func newEtcdClient() (client *etcd.Client) {
	maxConnectRetries := 12
	for maxConnectRetries > 0 {
		if _, _, err := tools.GetEtcdVersion(*etcd_server); err != nil {
			log.Fatalf("Failed to connect to etcd server: %v, error: %v", *etcd_server, err)
			if maxConnectRetries > 0 {
				log.Println("Retrying request after 5 second sleep.")
				time.Sleep(5 * time.Second)
				maxConnectRetries--
			} else {
				return nil
			}
		} else {
			log.Printf("Etcd server found: %v", *etcd_server)
			break
		}
	}
	client = etcd.NewClient([]string{*etcd_server})
	if client == nil {
		return nil
	}
	client.SyncCluster()

	return client
}

// TODO: evaluate using pkg/client/clientcmd
func newKubeClient() (*kclient.Client, error) {
	config := &kclient.Config{}

	masterHost := os.Getenv("KUBERNETES_RO_SERVICE_HOST")
	if masterHost == "" {
		log.Fatalf("KUBERNETES_RO_SERVICE_HOST is not defined")
	}
	masterPort := os.Getenv("KUBERNETES_RO_SERVICE_PORT")
	if masterPort == "" {
		log.Fatalf("KUBERNETES_RO_SERVICE_PORT is not defined")
	}
	config.Host = fmt.Sprintf("http://%s:%s", masterHost, masterPort)
	log.Printf("Using %s for kubernetes master", config.Host)

	config.Version = "v1beta1"
	log.Printf("Using kubernetes API %s", config.Version)

	return kclient.New(config)
}

func buildNameString(service, namespace, domain string) string {
	return fmt.Sprintf("%s.%s.%s.", service, namespace, domain)
}

func watchOnce(etcdClient *etcd.Client, kubeClient *kclient.Client) {
	// Start the goroutine to produce update events.
	updates := make(chan serviceUpdate)
	startWatching(kubeClient.Services(kapi.NamespaceAll), updates)

	// This loop will break if the channel closes, which is how the
	// goroutine signals an error.
	for ev := range updates {
		if *verbose {
			log.Printf("Received update event: %#v", ev)
		}
		switch ev.Op {
		case SetServices, AddService:
			for i := range ev.Services {
				s := &ev.Services[i]
				name := buildNameString(s.Name, s.Namespace, *domain)
				mutateEtcdOrDie(func() error { return addDNS(name, s, etcdClient) })
			}
		case RemoveService:
			for i := range ev.Services {
				s := &ev.Services[i]
				name := buildNameString(s.Name, s.Namespace, *domain)
				mutateEtcdOrDie(func() error { return removeDNS(name, etcdClient) })
			}
		}
	}
	//TODO: fully resync periodically.
}

func main() {
	flag.Parse()

	etcdClient := newEtcdClient()
	if etcdClient == nil {
		log.Fatal("Failed to create etcd client")
	}

	kubeClient, err := newKubeClient()
	if err != nil {
		log.Fatalf("Failed to create a kubernetes client: %v", err)
	}

	// In case of error, the watch will be aborted.  At that point we just
	// retry.
	for {
		watchOnce(etcdClient, kubeClient)
	}
}

//FIXME: make the below part of the k8s client lib?

// servicesWatcher is capable of listing and watching for changes to services
// across ALL namespaces
type servicesWatcher interface {
	List(label klabels.Selector) (*kapi.ServiceList, error)
	Watch(label klabels.Selector, field kfields.Selector, resourceVersion string) (kwatch.Interface, error)
}

type operation int

// These are the available operation types.
const (
	SetServices operation = iota
	AddService
	RemoveService
)

// serviceUpdate describes an operation of services, sent on the channel.
//
// You can add or remove a single service by sending an array of size one with
// Op == AddService|RemoveService.  For setting the state of the system to a given state, just
// set Services as desired and Op to SetServices, which will reset the system
// state to that specified in this operation for this source channel. To remove
// all services, set Services to empty array and Op to SetServices
type serviceUpdate struct {
	Services []kapi.Service
	Op       operation
}

// startWatching launches a goroutine that watches for changes to services.
func startWatching(watcher servicesWatcher, updates chan<- serviceUpdate) {
	serviceVersion := ""
	go watchLoop(watcher, updates, &serviceVersion)
}

// watchLoop loops forever looking for changes to services.  If an error occurs
// it will close the channel and return.
func watchLoop(svcWatcher servicesWatcher, updates chan<- serviceUpdate, resourceVersion *string) {
	defer close(updates)

	if len(*resourceVersion) == 0 {
		services, err := svcWatcher.List(klabels.Everything())
		if err != nil {
			log.Printf("Failed to load services: %v", err)
			return
		}
		*resourceVersion = services.ResourceVersion
		updates <- serviceUpdate{Op: SetServices, Services: services.Items}
	}

	watcher, err := svcWatcher.Watch(klabels.Everything(), kfields.Everything(), *resourceVersion)
	if err != nil {
		log.Printf("Failed to watch for service changes: %v", err)
		return
	}
	defer watcher.Stop()

	ch := watcher.ResultChan()
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				log.Printf("watchLoop channel closed")
				return
			}

			if event.Type == kwatch.Error {
				if status, ok := event.Object.(*kapi.Status); ok {
					log.Printf("Error during watch: %#v", status)
					return
				}
				log.Fatalf("Received unexpected error: %#v", event.Object)
			}

			if service, ok := event.Object.(*kapi.Service); ok {
				sendUpdate(updates, event, service, resourceVersion)
				continue
			}
		}
	}
}

func sendUpdate(updates chan<- serviceUpdate, event kwatch.Event, service *kapi.Service, resourceVersion *string) {
	*resourceVersion = service.ResourceVersion

	switch event.Type {
	case kwatch.Added, kwatch.Modified:
		updates <- serviceUpdate{Op: AddService, Services: []kapi.Service{*service}}
	case kwatch.Deleted:
		updates <- serviceUpdate{Op: RemoveService, Services: []kapi.Service{*service}}
	default:
		log.Fatalf("Unknown event.Type: %v", event.Type)
	}
}
