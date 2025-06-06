/*
Copyright 2017 The Kubernetes Authors.

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

package source

import (
	"context"
	"fmt"
	"text/template"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kubeinformers "k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"sigs.k8s.io/external-dns/endpoint"
)

const warningMsg = "The default behavior of exposing internal IPv6 addresses will change in the next minor version. Use --no-expose-internal-ipv6 flag to opt-in to the new behavior."

type nodeSource struct {
	client               kubernetes.Interface
	annotationFilter     string
	fqdnTemplate         *template.Template
	nodeInformer         coreinformers.NodeInformer
	labelSelector        labels.Selector
	excludeUnschedulable bool
	exposeInternalIPV6   bool
}

// NewNodeSource creates a new nodeSource with the given config.
func NewNodeSource(ctx context.Context, kubeClient kubernetes.Interface, annotationFilter, fqdnTemplate string, labelSelector labels.Selector, exposeInternalIPv6 bool, excludeUnschedulable bool) (Source, error) {
	tmpl, err := parseTemplate(fqdnTemplate)
	if err != nil {
		return nil, err
	}

	// Use shared informers to listen for add/update/delete of nodes.
	// Set resync period to 0, to prevent processing when nothing has changed
	informerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(kubeClient, 0)
	nodeInformer := informerFactory.Core().V1().Nodes()

	// Add default resource event handler to properly initialize informer.
	nodeInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				log.Debug("node added")
			},
		},
	)

	informerFactory.Start(ctx.Done())

	// wait for the local cache to be populated.
	if err := waitForCacheSync(context.Background(), informerFactory); err != nil {
		return nil, err
	}

	return &nodeSource{
		client:               kubeClient,
		annotationFilter:     annotationFilter,
		fqdnTemplate:         tmpl,
		nodeInformer:         nodeInformer,
		labelSelector:        labelSelector,
		excludeUnschedulable: excludeUnschedulable,
		exposeInternalIPV6:   exposeInternalIPv6,
	}, nil
}

// Endpoints returns endpoint objects for each service that should be processed.
func (ns *nodeSource) Endpoints(ctx context.Context) ([]*endpoint.Endpoint, error) {
	nodes, err := ns.nodeInformer.Lister().List(ns.labelSelector)
	if err != nil {
		return nil, err
	}

	nodes, err = ns.filterByAnnotations(nodes)
	if err != nil {
		return nil, err
	}

	endpoints := map[endpoint.EndpointKey]*endpoint.Endpoint{}

	// create endpoints for all nodes
	for _, node := range nodes {
		// Check controller annotation to see if we are responsible.
		controller, ok := node.Annotations[controllerAnnotationKey]
		if ok && controller != controllerAnnotationValue {
			log.Debugf("Skipping node %s because controller value does not match, found: %s, required: %s",
				node.Name, controller, controllerAnnotationValue)
			continue
		}

		if node.Spec.Unschedulable && ns.excludeUnschedulable {
			log.Debugf("Skipping node %s because it is unschedulable", node.Name)
			continue
		}

		log.Debugf("creating endpoint for node %s", node.Name)

		ttl := getTTLFromAnnotations(node.Annotations, fmt.Sprintf("node/%s", node.Name))

		// create new endpoint with the information we already have
		ep := &endpoint.Endpoint{
			RecordTTL: ttl,
		}

		if ns.fqdnTemplate != nil {
			hostnames, err := execTemplate(ns.fqdnTemplate, node)
			if err != nil {
				return nil, err
			}
			hostname := ""
			if len(hostnames) > 0 {
				hostname = hostnames[0]
			}
			ep.DNSName = hostname
			log.Debugf("applied template for %s, converting to %s", node.Name, ep.DNSName)
		} else {
			ep.DNSName = node.Name
			log.Debugf("not applying template for %s", node.Name)
		}

		addrs := getTargetsFromTargetAnnotation(node.Annotations)
		if len(addrs) == 0 {
			addrs, err = ns.nodeAddresses(node)
			if err != nil {
				return nil, fmt.Errorf("failed to get node address from %s: %w", node.Name, err)
			}
		}

		ep.Labels = endpoint.NewLabels()
		for _, addr := range addrs {
			log.Debugf("adding endpoint %s target %s", ep, addr)
			key := endpoint.EndpointKey{
				DNSName:    ep.DNSName,
				RecordType: suitableType(addr),
			}
			if _, ok := endpoints[key]; !ok {
				epCopy := *ep
				epCopy.RecordType = key.RecordType
				endpoints[key] = &epCopy
			}
			endpoints[key].Targets = append(endpoints[key].Targets, addr)
		}
	}

	endpointsSlice := []*endpoint.Endpoint{}
	for _, ep := range endpoints {
		endpointsSlice = append(endpointsSlice, ep)
	}

	return endpointsSlice, nil
}

func (ns *nodeSource) AddEventHandler(ctx context.Context, handler func()) {
}

// nodeAddress returns node's externalIP and if that's not found, node's internalIP
// basically what k8s.io/kubernetes/pkg/util/node.GetPreferredNodeAddress does
func (ns *nodeSource) nodeAddresses(node *v1.Node) ([]string, error) {
	addresses := map[v1.NodeAddressType][]string{
		v1.NodeExternalIP: {},
		v1.NodeInternalIP: {},
	}
	var internalIpv6Addresses []string

	for _, addr := range node.Status.Addresses {
		// IPv6 InternalIP addresses have special handling.
		// Refer to https://github.com/kubernetes-sigs/external-dns/pull/5192 for more details.
		if addr.Type == v1.NodeInternalIP && suitableType(addr.Address) == endpoint.RecordTypeAAAA {
			internalIpv6Addresses = append(internalIpv6Addresses, addr.Address)
		}
		addresses[addr.Type] = append(addresses[addr.Type], addr.Address)
	}

	if len(addresses[v1.NodeExternalIP]) > 0 {
		if ns.exposeInternalIPV6 {
			log.Warn(warningMsg)
			return append(addresses[v1.NodeExternalIP], internalIpv6Addresses...), nil
		}
		return addresses[v1.NodeExternalIP], nil
	}

	if len(addresses[v1.NodeInternalIP]) > 0 {
		return addresses[v1.NodeInternalIP], nil
	}

	return nil, fmt.Errorf("could not find node address for %s", node.Name)
}

// filterByAnnotations filters a list of nodes by a given annotation selector.
func (ns *nodeSource) filterByAnnotations(nodes []*v1.Node) ([]*v1.Node, error) {
	labelSelector, err := metav1.ParseToLabelSelector(ns.annotationFilter)
	if err != nil {
		return nil, err
	}
	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return nil, err
	}

	// empty filter returns original list
	if selector.Empty() {
		return nodes, nil
	}

	filteredList := []*v1.Node{}

	for _, node := range nodes {
		// convert the node's annotations to an equivalent label selector
		annotations := labels.Set(node.Annotations)

		// include node if its annotations match the selector
		if selector.Matches(annotations) {
			filteredList = append(filteredList, node)
		}
	}

	return filteredList, nil
}
