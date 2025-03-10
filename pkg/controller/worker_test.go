package controller

import (
	"container/list"
	"context"
	"encoding/json"
	"github.com/F5Networks/k8s-bigip-ctlr/v2/pkg/resource"
	"github.com/F5Networks/k8s-bigip-ctlr/v2/pkg/teem"
	routeapi "github.com/openshift/api/route/v1"
	fakeRouteClient "github.com/openshift/client-go/route/clientset/versioned/fake"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/workqueue"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	ficV1 "github.com/F5Networks/f5-ipam-controller/pkg/ipamapis/apis/fic/v1"
	"github.com/F5Networks/f5-ipam-controller/pkg/ipammachinery"
	crdfake "github.com/F5Networks/k8s-bigip-ctlr/v2/config/client/clientset/versioned/fake"
	cisinfv1 "github.com/F5Networks/k8s-bigip-ctlr/v2/config/client/informers/externalversions/cis/v1"
	apm "github.com/F5Networks/k8s-bigip-ctlr/v2/pkg/appmanager"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	cisapiv1 "github.com/F5Networks/k8s-bigip-ctlr/v2/config/apis/cis/v1"
	"github.com/F5Networks/k8s-bigip-ctlr/v2/pkg/test"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Worker Tests", func() {
	var mockCtlr *mockController
	var vrt1 *cisapiv1.VirtualServer
	var svc1 *v1.Service
	namespace := "default"

	BeforeEach(func() {
		mockCtlr = newMockController()
		svc1 = test.NewService(
			"svc1",
			"1",
			namespace,
			v1.ServiceTypeClusterIP,
			[]v1.ServicePort{
				{
					Port: 80,
					Name: "port0",
				},
			},
		)

		vrt1 = test.NewVirtualServer(
			"SampleVS",
			namespace,
			cisapiv1.VirtualServerSpec{
				Host:                   "test.com",
				VirtualServerAddress:   "1.2.3.4",
				IPAMLabel:              "",
				VirtualServerName:      "",
				VirtualServerHTTPPort:  0,
				VirtualServerHTTPSPort: 0,
				Pools: []cisapiv1.Pool{
					cisapiv1.Pool{
						Path:    "/path",
						Service: "svc1",
					},
				},
				TLSProfileName:   "",
				HTTPTraffic:      "",
				SNAT:             "",
				WAF:              "",
				RewriteAppRoot:   "",
				AllowVLANs:       nil,
				IRules:           nil,
				ServiceIPAddress: nil,
			})
		mockCtlr.Partition = "test"
		mockCtlr.Agent = &Agent{
			PostManager: &PostManager{
				PostParams: PostParams{
					BIGIPURL: "10.10.10.1",
				},
			},
		}
		mockCtlr.kubeCRClient = crdfake.NewSimpleClientset(vrt1)
		mockCtlr.kubeClient = k8sfake.NewSimpleClientset(svc1)
		mockCtlr.mode = CustomResourceMode
		mockCtlr.crInformers = make(map[string]*CRInformer)
		mockCtlr.comInformers = make(map[string]*CommonInformer)
		mockCtlr.nativeResourceSelector, _ = createLabelSelector(DefaultCustomResourceLabel)
		_ = mockCtlr.addNamespacedInformers("default", false)
		mockCtlr.resources = NewResourceStore()
		mockCtlr.crInformers["default"].vsInformer = cisinfv1.NewFilteredVirtualServerInformer(
			mockCtlr.kubeCRClient,
			namespace,
			0,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			func(options *metav1.ListOptions) {
				options.LabelSelector = mockCtlr.nativeResourceSelector.String()
			},
		)
		mockCtlr.crInformers["default"].ilInformer = cisinfv1.NewFilteredIngressLinkInformer(
			mockCtlr.kubeCRClient,
			namespace,
			0,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			func(options *metav1.ListOptions) {
				options.LabelSelector = mockCtlr.nativeResourceSelector.String()
			},
		)
	})

	Describe("Validating Ingress link functions", func() {
		var namespace string
		BeforeEach(func() {
			namespace = "nginx-ingress"
		})

		It("Validating filterIngressLinkForService filters the correct ingresslink resource", func() {
			fooPorts := []v1.ServicePort{
				{
					Port: 8080,
					Name: "port0",
				},
			}
			foo := test.NewService("foo", "1", namespace, v1.ServiceTypeClusterIP, fooPorts)
			label1 := make(map[string]string)
			label2 := make(map[string]string)
			label1["app"] = "ingresslink"
			label2["app"] = "dummy"
			foo.ObjectMeta.Labels = label1
			var (
				selctor = &metav1.LabelSelector{
					MatchLabels: label1,
				}
			)
			var iRules []string
			IngressLink1 := test.NewIngressLink("ingresslink1", namespace, "1",
				cisapiv1.IngressLinkSpec{
					VirtualServerAddress: "",
					Selector:             selctor,
					IRules:               iRules,
				})
			IngressLink2 := test.NewIngressLink("ingresslink2", "dummy", "1",
				cisapiv1.IngressLinkSpec{
					VirtualServerAddress: "",
					Selector:             selctor,
					IRules:               iRules,
				})
			var IngressLinks []*cisapiv1.IngressLink
			IngressLinks = append(IngressLinks, IngressLink1, IngressLink2)
			ingresslinksForService := filterIngressLinkForService(IngressLinks, foo)
			Expect(ingresslinksForService[0]).To(Equal(IngressLink1), "Should return the Ingresslink1 object")
		})
		It("Validating service are sorted properly", func() {
			fooPorts := []v1.ServicePort{
				{
					Port: 8080,
					Name: "port0",
				},
			}
			foo := test.NewService("foo", "1", namespace, v1.ServiceTypeClusterIP, fooPorts)
			bar := test.NewService("bar", "1", namespace, v1.ServiceTypeClusterIP, fooPorts)
			bar.ObjectMeta.CreationTimestamp = metav1.NewTime(time.Now())
			time.Sleep(10 * time.Millisecond)
			foo.ObjectMeta.CreationTimestamp = metav1.NewTime(time.Now())
			var services Services
			services = append(services, foo, bar)
			sort.Sort(services)
			Expect(services[0].Name).To(Equal("bar"), "Should return the service name as bar")
		})
	})

	Describe("IPAM", func() {
		DEFAULT_PARTITION = "Test"
		BeforeEach(func() {
			mockCtlr.Agent = &Agent{
				PostManager: &PostManager{
					PostParams: PostParams{
						BIGIPURL: "10.10.10.1",
					},
				},
			}
			mockCtlr.ipamCli = ipammachinery.NewFakeIPAMClient(nil, nil, nil)
		})

		It("Create IPAM Custom Resource", func() {
			err := mockCtlr.createIPAMResource()
			Expect(err).To(BeNil(), "Failed to Create IPAM Custom Resource")
			err = mockCtlr.createIPAMResource()
			Expect(err).To(BeNil(), "Failed to Create IPAM Custom Resource")

		})

		It("Get IPAM Resource", func() {
			_ = mockCtlr.createIPAMResource()
			ipamCR := mockCtlr.getIPAMCR()
			Expect(ipamCR).NotTo(BeNil(), "Failed to GET IPAM")
			mockCtlr.ipamCR = mockCtlr.ipamCR + "invalid"
			ipamCR = mockCtlr.getIPAMCR()
			Expect(ipamCR).To(BeNil(), "Failed to GET IPAM")
			mockCtlr.ipamCR = mockCtlr.ipamCR + "/invalid"
			ipamCR = mockCtlr.getIPAMCR()
			Expect(ipamCR).To(BeNil(), "Failed to GET IPAM")
		})

		It("Request IP Address", func() {

			testSpec := make(map[string]string)
			testSpec["host"] = "foo.com"
			testSpec["key"] = "ns/name"

			for sp, val := range testSpec {
				_ = mockCtlr.createIPAMResource()
				var key, host, errHint string
				if sp == "host" {
					host = val
					key = ""
					errHint = "Host: "
				} else {
					key = val
					host = ""
					errHint = "Key: "
				}

				ip, status := mockCtlr.requestIP("test", host, key)
				Expect(status).To(Equal(Requested), errHint+"Failed to Request IP")
				Expect(ip).To(BeEmpty(), errHint+"IP available even before requesting")
				ipamCR := mockCtlr.getIPAMCR()
				Expect(len(ipamCR.Spec.HostSpecs)).To(Equal(1), errHint+"Invalid number of Host Specs")
				Expect(ipamCR.Spec.HostSpecs[0].IPAMLabel).To(Equal("test"), errHint+"IPAM Request Failed")
				Expect(ipamCR.Spec.HostSpecs[0].Host).To(Equal(host), errHint+"IPAM Request Failed")
				Expect(ipamCR.Spec.HostSpecs[0].Key).To(Equal(key), errHint+"IPAM Request Failed")

				ip, status = mockCtlr.requestIP("", host, key)
				Expect(status).To(Equal(InvalidInput), errHint+"Failed to validate invalid input")
				Expect(ip).To(BeEmpty(), errHint+"Failed to validate invalid input")
				newIPAMCR := mockCtlr.getIPAMCR()
				Expect(reflect.DeepEqual(ipamCR, newIPAMCR)).To(BeTrue(), errHint+"IPAM CR should not be updated")

				ip, status = mockCtlr.requestIP("test", host, key)
				Expect(status).To(Equal(Requested), errHint+"Wrong status")
				Expect(ip).To(BeEmpty(), errHint+"Invalid IP")
				newIPAMCR = mockCtlr.getIPAMCR()
				Expect(reflect.DeepEqual(ipamCR, newIPAMCR)).To(BeTrue(), errHint+"IPAM CR should not be updated")

				ipamCR.Status.IPStatus = []*ficV1.IPSpec{
					{
						IPAMLabel: "test",
						Host:      host,
						IP:        "10.10.10.1",
						Key:       key,
					},
				}
				ipamCR, _ = mockCtlr.ipamCli.Update(ipamCR)
				ip, status = mockCtlr.requestIP("test", host, key)
				Expect(ip).To(Equal("10.10.10.1"), errHint+"Invalid IP")
				Expect(status).To(Equal(Allocated), "Failed to fetch Allocated IP")
				ipamCR = mockCtlr.getIPAMCR()
				Expect(len(ipamCR.Spec.HostSpecs)).To(Equal(1), errHint+"Invalid number of Host Specs")
				Expect(ipamCR.Spec.HostSpecs[0].IPAMLabel).To(Equal("test"), errHint+"IPAM Request Failed")
				Expect(ipamCR.Spec.HostSpecs[0].Host).To(Equal(host), errHint+"IPAM Request Failed")
				Expect(ipamCR.Spec.HostSpecs[0].Key).To(Equal(key), errHint+"IPAM Request Failed")

				ip, status = mockCtlr.requestIP("dev", host, key)
				Expect(status).To(Equal(Requested), "Failed to Request IP")
				Expect(ip).To(BeEmpty(), errHint+"Invalid IP")
				ipamCR = mockCtlr.getIPAMCR()
				// TODO: The expected number of Specs is 1. After the bug gets fixed update this to 1 from 2.
				Expect(len(ipamCR.Spec.HostSpecs)).To(Equal(2), errHint+"Invalid number of Host Specs")
				Expect(ipamCR.Spec.HostSpecs[0].Host).To(Equal(host), errHint+"IPAM Request Failed")
				Expect(ipamCR.Spec.HostSpecs[0].Key).To(Equal(key), errHint+"IPAM Request Failed")

				ip, status = mockCtlr.requestIP("test", "", "")
				Expect(status).To(Equal(InvalidInput), errHint+"Failed to validate invalid input")
				Expect(ip).To(BeEmpty(), errHint+"Invalid IP")
				newIPAMCR = mockCtlr.getIPAMCR()
				Expect(reflect.DeepEqual(ipamCR, newIPAMCR)).To(BeTrue(), errHint+"IPAM CR should not be updated")

				ipamCR.Spec.HostSpecs = []*ficV1.HostSpec{}
				ipamCR.Status.IPStatus = []*ficV1.IPSpec{
					{
						IPAMLabel: "old",
						Host:      host,
						IP:        "10.10.10.2",
						Key:       key,
					},
				}
				ipamCR, _ = mockCtlr.ipamCli.Update(ipamCR)

				ip, status = mockCtlr.requestIP("old", host, key)
				Expect(ip).To(Equal(""), errHint+"Invalid IP")
				Expect(status).To(Equal(NotRequested), "Failed to identify Stale status")
			}
		})

		It("Release IP Addresss", func() {
			testSpec := make(map[string]string)
			testSpec["host"] = "foo.com"
			testSpec["key"] = "ns/name"

			for sp, val := range testSpec {
				_ = mockCtlr.createIPAMResource()
				var key, host, errHint string
				if sp == "host" {
					host = val
					key = ""
					errHint = "Host: "
				} else {
					key = val
					host = ""
					errHint = "Key: "
				}

				ip := mockCtlr.releaseIP("", host, key)
				Expect(ip).To(BeEmpty(), errHint+"Unexpected IP address released")

				ipamCR := mockCtlr.getIPAMCR()
				ipamCR.Spec.HostSpecs = []*ficV1.HostSpec{
					{
						IPAMLabel: "test",
						Host:      host,
						Key:       key,
					},
				}
				ipamCR.Status.IPStatus = []*ficV1.IPSpec{
					{
						IPAMLabel: "test",
						Host:      host,
						IP:        "10.10.10.1",
						Key:       key,
					},
				}
				ipamCR, _ = mockCtlr.ipamCli.Update(ipamCR)

				ip = mockCtlr.releaseIP("test", host, key)
				ipamCR = mockCtlr.getIPAMCR()
				Expect(len(ipamCR.Spec.HostSpecs)).To(Equal(0), errHint+"IP Address Not released")
				Expect(ip).To(Equal("10.10.10.1"), errHint+"Wrong IP Address released")
			}
		})

		It("IPAM Label", func() {
			vrt2 := test.NewVirtualServer(
				"SampleVS2",
				namespace,
				cisapiv1.VirtualServerSpec{
					Host: "test.com",
					Pools: []cisapiv1.Pool{
						cisapiv1.Pool{
							Path:    "/path",
							Service: "svc1",
						},
					},
				})
			vrt3 := test.NewVirtualServer(
				"SampleVS3",
				namespace,
				cisapiv1.VirtualServerSpec{
					Host: "test.com",
					Pools: []cisapiv1.Pool{
						cisapiv1.Pool{
							Path:    "/path2",
							Service: "svc2",
						},
					},
				})
			label := getIPAMLabel([]*cisapiv1.VirtualServer{vrt2, vrt3})
			Expect(label).To(BeEmpty())
			vrt3.Spec.IPAMLabel = "test"
			label = getIPAMLabel([]*cisapiv1.VirtualServer{vrt2, vrt3})
			Expect(label).To(Equal("test"))
		})
	})

	Describe("Filtering and Validation", func() {
		It("Filter VS for Service", func() {
			ns := "temp"
			svc := test.NewService("svc", "1", ns, v1.ServiceTypeClusterIP, nil)
			vrt2 := test.NewVirtualServer(
				"SampleVS2",
				ns,
				cisapiv1.VirtualServerSpec{
					Host:                 "test2.com",
					VirtualServerAddress: "1.2.3.5",
					Pools: []cisapiv1.Pool{
						cisapiv1.Pool{
							Path:    "/path",
							Service: "svc",
						},
					},
				})
			vrt3 := test.NewVirtualServer(
				"SampleVS",
				ns,
				cisapiv1.VirtualServerSpec{
					Host:                 "test3.com",
					VirtualServerAddress: "1.2.3.6",
					Pools: []cisapiv1.Pool{
						cisapiv1.Pool{
							Path:    "/path",
							Service: "svc",
						},
					},
				})
			res := filterVirtualServersForService([]*cisapiv1.VirtualServer{vrt1, vrt2, vrt3}, svc)
			Expect(len(res)).To(Equal(2), "Wrong list of Virtual Servers")
			Expect(res[0]).To(Equal(vrt2), "Wrong list of Virtual Servers")
			Expect(res[1]).To(Equal(vrt3), "Wrong list of Virtual Servers")
		})
		It("Filter TS for Service", func() {
			ns := "temp"
			svc := test.NewService("svc", "1", ns, v1.ServiceTypeClusterIP, nil)

			ts1 := test.NewTransportServer(
				"SampleTS1",
				namespace,
				cisapiv1.TransportServerSpec{
					Pool: cisapiv1.Pool{
						Path:    "/path",
						Service: "svc",
					},
				},
			)
			ts2 := test.NewTransportServer(
				"SampleTS1",
				ns,
				cisapiv1.TransportServerSpec{
					Pool: cisapiv1.Pool{
						Path:    "/path",
						Service: "svc",
					},
				},
			)
			ts3 := test.NewTransportServer(
				"SampleTS1",
				ns,
				cisapiv1.TransportServerSpec{
					Pool: cisapiv1.Pool{
						Path:    "/path",
						Service: "svc1",
					},
				},
			)

			res := filterTransportServersForService([]*cisapiv1.TransportServer{ts1, ts2, ts3}, svc)
			Expect(len(res)).To(Equal(1), "Wrong list of Transport Servers")
			Expect(res[0]).To(Equal(ts2), "Wrong list of Transport Servers")
		})

		It("Filter VS for TLSProfile", func() {
			tlsProf := test.NewTLSProfile("sampleTLS", namespace, cisapiv1.TLSProfileSpec{
				Hosts: []string{"test2.com"},
			})
			vrt2 := test.NewVirtualServer(
				"SampleVS2",
				namespace,
				cisapiv1.VirtualServerSpec{
					Host:                 "test2.com",
					VirtualServerAddress: "1.2.3.5",
					TLSProfileName:       "sampleTLS",
				})
			vrt3 := test.NewVirtualServer(
				"SampleVS",
				namespace,
				cisapiv1.VirtualServerSpec{
					Host:                 "test2.com",
					VirtualServerAddress: "1.2.3.5",
					TLSProfileName:       "sampleTLS",
				})
			res := getVirtualServersForTLSProfile([]*cisapiv1.VirtualServer{vrt1, vrt2, vrt3}, tlsProf)
			Expect(len(res)).To(Equal(2), "Wrong list of Virtual Servers")
			Expect(res[0]).To(Equal(vrt2), "Wrong list of Virtual Servers")
			Expect(res[1]).To(Equal(vrt3), "Wrong list of Virtual Servers")
		})

		It("VS Handling HTTP", func() {
			Expect(doesVSHandleHTTP(vrt1)).To(BeTrue(), "HTTP VS in invalid")
			vrt1.Spec.TLSProfileName = "TLSProf"
			Expect(doesVSHandleHTTP(vrt1)).To(BeFalse(), "HTTPS VS in invalid")
			vrt1.Spec.HTTPTraffic = TLSAllowInsecure
			Expect(doesVSHandleHTTP(vrt1)).To(BeTrue(), "HTTPS VS in invalid")
		})

		Describe("Filter Associated VirtualServers", func() {
			var vrt2, vrt3, vrt4 *cisapiv1.VirtualServer
			BeforeEach(func() {
				vrt2 = test.NewVirtualServer(
					"SampleVS2",
					namespace,
					cisapiv1.VirtualServerSpec{
						Host:                 "test2.com",
						VirtualServerAddress: "1.2.3.5",
						Pools: []cisapiv1.Pool{
							cisapiv1.Pool{
								Path:    "/path",
								Service: "svc",
							},
						},
					})
				vrt3 = test.NewVirtualServer(
					"SampleVS3",
					namespace,
					cisapiv1.VirtualServerSpec{
						Host:                 "test2.com",
						VirtualServerAddress: "1.2.3.5",
						Pools: []cisapiv1.Pool{
							cisapiv1.Pool{
								Path:    "/path3",
								Service: "svc",
							},
						},
					})
				vrt4 = test.NewVirtualServer(
					"SampleVS4",
					namespace,
					cisapiv1.VirtualServerSpec{
						Host:                 "test2.com",
						VirtualServerAddress: "1.2.3.5",
						Pools: []cisapiv1.Pool{
							cisapiv1.Pool{
								Path:    "/path4",
								Service: "svc",
							},
						},
					})
			})
			It("Duplicate Paths", func() {
				vrt3.Spec.Pools[0].Path = "/path"
				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt3},
					false)
				Expect(len(virts)).To(Equal(1), "Wrong number of Virtual Servers")
				Expect(virts[0].Name).To(Equal("SampleVS2"), "Wrong Virtual Server")
			})

			It("Unassociated VS", func() {
				vrt4.Spec.Host = "new.com"
				vrt4.Spec.VirtualServerAddress = "1.2.3.6"
				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt4},
					false)
				Expect(len(virts)).To(Equal(1), "Wrong number of Virtual Servers")
				Expect(virts[0].Name).To(Equal("SampleVS2"), "Wrong Virtual Server")
			})

			It("Unique Paths", func() {
				//vrt3.Spec.Pools[0].Path = "/path3"
				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt3},
					false)
				Expect(len(virts)).To(Equal(2), "Wrong number of Virtual Servers")
				Expect(virts[0].Name).To(Equal("SampleVS2"), "Wrong Virtual Server")
				Expect(virts[1].Name).To(Equal("SampleVS3"), "Wrong Virtual Server")
			})

			It("Deletion", func() {
				//vrt3.Spec.Pools[0].Path = "/path3"
				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt3},
					true)
				Expect(len(virts)).To(Equal(1), "Wrong number of Virtual Servers")
				Expect(virts[0].Name).To(Equal("SampleVS3"), "Wrong Virtual Server")
			})

			It("Re-adding VirtualServer", func() {
				vrt2 = test.NewVirtualServer(
					"SampleVS2",
					namespace,
					cisapiv1.VirtualServerSpec{
						Host:                 "test2.com",
						VirtualServerAddress: "1.2.3.5",
						Pools: []cisapiv1.Pool{
							cisapiv1.Pool{
								Path:    "/path",
								Service: "svc",
							},
						},
					})
				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt3},
					false)
				Expect(len(virts)).To(Equal(2), "Wrong number of Virtual Servers")
				Expect(virts[0].Name).To(Equal("SampleVS2"), "Wrong Virtual Server")
				Expect(virts[1].Name).To(Equal("SampleVS3"), "Wrong Virtual Server")
			})

			It("Re-Deletion of VS", func() {
				//vrt3.Spec.Pools[0].Path = "/path3"
				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt3},
					true)
				Expect(len(virts)).To(Equal(1), "Wrong number of Virtual Servers")
				Expect(virts[0].Name).To(Equal("SampleVS3"), "Wrong Virtual Server")
			})

			It("Absence of HostName of Unassociated VS", func() {
				vrt3.Spec.Host = ""
				//vrt3.Spec.Pools[0].Path = "/path3"
				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt3},
					false)
				Expect(len(virts)).To(Equal(1), "Wrong number of Virtual Servers")
				Expect(virts[0].Name).To(Equal("SampleVS2"), "Wrong Virtual Server")
			})

			It("Absence of HostName of Associated VS", func() {
				vrt3.Spec.Host = ""
				//vrt3.Spec.Pools[0].Path = "/path3"
				vrt4.Spec.Host = ""

				virts := mockCtlr.getAssociatedVirtualServers(vrt3,
					[]*cisapiv1.VirtualServer{vrt2, vrt3, vrt4},
					false)
				Expect(len(virts)).To(Equal(2), "Wrong number of Virtual Servers")
				Expect(virts[0].Name).To(Equal("SampleVS3"), "Wrong Virtual Server")
				Expect(virts[1].Name).To(Equal("SampleVS4"), "Wrong Virtual Server")
			})

			It("UnAssociated VS 2", func() {
				vrt3.Spec.Host = ""
				//vrt3.Spec.Pools[0].Path = "/path3"
				vrt4.Spec.Host = ""
				vrt4.Spec.VirtualServerAddress = "1.2.3.6"

				virts := mockCtlr.getAssociatedVirtualServers(vrt3,
					[]*cisapiv1.VirtualServer{vrt2, vrt3, vrt4},
					false)
				Expect(len(virts)).To(Equal(1), "Wrong number of Virtual Servers")
				Expect(virts[0].Name).To(Equal("SampleVS3"), "Wrong Virtual Server")
			})

			It("Virtuals with same Host, but different Virtual Address", func() {
				vrt4.Spec.Host = "test2.com"
				vrt4.Spec.VirtualServerAddress = "1.2.3.6"

				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt4},
					false)
				Expect(virts).To(BeNil(), "Wrong Number of Virtual Servers")
			})

			It("HostGroup", func() {
				vrt2.Spec.HostGroup = "test"
				vrt3.Spec.HostGroup = "test"
				vrt3.Spec.Host = "test3.com"

				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt3, vrt4},
					false)
				Expect(len(virts)).To(Equal(2), "Wrong number of Virtual Servers")
				Expect(virts[0].Spec.Host).To(Equal("test2.com"), "Wrong Virtual Server Host")
				Expect(virts[1].Spec.Host).To(Equal("test3.com"), "Wrong Virtual Server Host")
			})

			It("Host Group with IP Address Only specified once", func() {
				vrt2.Spec.HostGroup = "test"
				vrt3.Spec.HostGroup = "test"
				vrt3.Spec.Host = "test3.com"
				vrt3.Spec.VirtualServerAddress = ""

				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt3, vrt4},
					false)

				Expect(len(virts)).To(Equal(2), "Wrong number of Virtual Servers")
				Expect(virts[0].Spec.Host).To(Equal("test2.com"), "Wrong Virtual Server Host")
				Expect(virts[1].Spec.Host).To(Equal("test3.com"), "Wrong Virtual Server Host")
			})

			It("HostGroup with wrong custom port", func() {
				vrt2.Spec.HostGroup = "test"
				vrt2.Spec.VirtualServerHTTPPort = 8080

				vrt3.Spec.HostGroup = "test"
				vrt3.Spec.Host = "test3.com"
				vrt3.Spec.VirtualServerHTTPPort = 8081

				vrt4.Spec.HostGroup = "test"
				vrt4.Spec.Host = "test4.com"
				vrt4.Spec.VirtualServerHTTPPort = 8080

				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt3, vrt4},
					false)
				Expect(len(virts)).To(Equal(2), "Wrong number of Virtual Servers")
				Expect(virts[0].Name).To(Equal("SampleVS2"), "Wrong Virtual Server")
				Expect(virts[1].Name).To(Equal("SampleVS4"), "Wrong Virtual Server")
			})

			It("Unique Paths: same path but with different host names", func() {
				vrt2.Spec.HostGroup = "test"
				vrt2.Spec.Pools[0].Path = "/path"

				vrt3.Spec.HostGroup = "test"
				vrt3.Spec.Host = "test3.com"
				vrt3.Spec.Pools[0].Path = "/path"

				vrt4.Spec.HostGroup = "test"
				vrt4.Spec.Host = "test4.com"
				vrt4.Spec.Pools[0].Path = "/path"

				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt3, vrt4},
					false)
				Expect(len(virts)).To(Equal(3), "Wrong number of Virtual Servers")
				Expect(virts[0].Name).To(Equal("SampleVS2"), "Wrong Virtual Server")
				Expect(virts[1].Name).To(Equal("SampleVS3"), "Wrong Virtual Server")
				Expect(virts[2].Name).To(Equal("SampleVS4"), "Wrong Virtual Server")
			})

			It("IPAM Label", func() {
				mockCtlr.ipamCli = &ipammachinery.IPAMClient{}
				vrt2.Spec.IPAMLabel = "test"
				vrt3.Spec.IPAMLabel = "test"
				vrt4.Spec.IPAMLabel = "test"
				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt3, vrt4},
					false)
				Expect(len(virts)).To(Equal(3), "Wrong number of Virtual Servers")
				Expect(virts[0].Name).To(Equal("SampleVS2"), "Wrong Virtual Server")
				Expect(virts[1].Name).To(Equal("SampleVS3"), "Wrong Virtual Server")
				Expect(virts[2].Name).To(Equal("SampleVS4"), "Wrong Virtual Server")
			})

			It("IPAM Label: Absence in a virtualServer", func() {
				mockCtlr.ipamCli = &ipammachinery.IPAMClient{}
				vrt2.Spec.IPAMLabel = "test"
				vrt3.Spec.IPAMLabel = "test"
				vrt4.Spec.IPAMLabel = ""
				virts := mockCtlr.getAssociatedVirtualServers(vrt2,
					[]*cisapiv1.VirtualServer{vrt2, vrt3, vrt4},
					false)
				Expect(len(virts)).To(Equal(0), "Wrong number of Virtual Servers")
			})
			It("IPAM Label in a virtualServer with empty host", func() {
				mockCtlr.ipamCli = &ipammachinery.IPAMClient{}
				vrt4.Spec.IPAMLabel = "test"
				vrt4.Spec.Host = ""
				virts := mockCtlr.getAssociatedVirtualServers(vrt4,
					[]*cisapiv1.VirtualServer{vrt4},
					false)
				Expect(len(virts)).To(Equal(0), "Wrong number of Virtual Servers")
			})
			It("function getVirtualServerAddress", func() {
				address, err := getVirtualServerAddress([]*cisapiv1.VirtualServer{})
				Expect(address).To(Equal(""), "Should return empty virtual address")
				Expect(err).To(BeNil(), "error should be nil")
				vrt1.Spec.VirtualServerAddress = ""
				address, err = getVirtualServerAddress([]*cisapiv1.VirtualServer{vrt1})
				Expect(address).To(Equal(""), "Should return empty virtual address")
				Expect(err).ToNot(BeNil(), "error should not be nil")
				vrt1.Spec.VirtualServerAddress = "192.168.1.1"
				vrt2.Spec.VirtualServerAddress = "192.168.1.2"
				address, err = getVirtualServerAddress([]*cisapiv1.VirtualServer{vrt1, vrt2})
				Expect(address).To(Equal(""), "Should return empty virtual address")
				Expect(err).ToNot(BeNil(), "error should not be nil")
				address, err = getVirtualServerAddress([]*cisapiv1.VirtualServer{vrt1})
				Expect(address).To(Equal("192.168.1.1"), "Should not return empty virtual address")
				Expect(err).To(BeNil(), "error should be nil")
			})
		})
	})
	Describe("Endpoints", func() {
		BeforeEach(func() {
			mockCtlr.oldNodes = []Node{
				{
					Name: "worker1",
					Addr: "10.10.10.1",
					Labels: map[string]string{
						"worker": "true",
					},
				},
				{
					Name: "worker2",
					Addr: "10.10.10.2",
					Labels: map[string]string{
						"worker": "true",
					},
				},
				{
					Name: "master",
					Addr: "10.10.10.3",
				},
			}
		})

		It("NodePort", func() {
			var nodePort int32 = 30000
			members := []PoolMember{
				{
					Address: "10.10.10.1",
					Port:    nodePort,
					Session: "user-enabled",
				},
				{
					Address: "10.10.10.2",
					Port:    nodePort,
					Session: "user-enabled",
				},
				{
					Address: "10.10.10.3",
					Port:    nodePort,
					Session: "user-enabled",
				},
			}

			mems := mockCtlr.getEndpointsForNodePort(nodePort, "")
			Expect(mems).To(Equal(members), "Wrong set of Endpoints for NodePort")
			mems = mockCtlr.getEndpointsForNodePort(nodePort, "worker=true")
			Expect(mems).To(Equal(members[:2]), "Wrong set of Endpoints for NodePort")
			mems = mockCtlr.getEndpointsForNodePort(nodePort, "invalid label")
			Expect(len(mems)).To(Equal(0), "Wrong set of Endpoints for NodePort")
		})

	})

	Describe("Processing Resources", func() {
		It("Processing ServiceTypeLoadBalancer", func() {
			// Service when IPAM is not available
			_ = mockCtlr.processLBServices(svc1, false)
			Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Resource Config should be empty")

			mockCtlr.Agent = &Agent{
				PostManager: &PostManager{
					PostParams: PostParams{
						BIGIPURL: "10.10.10.1",
					},
				},
			}
			mockCtlr.Partition = "default"
			mockCtlr.ipamCli = ipammachinery.NewFakeIPAMClient(nil, nil, nil)
			mockCtlr.eventNotifier = apm.NewEventNotifier(nil)

			svc1.Spec.Type = v1.ServiceTypeLoadBalancer

			mockCtlr.resources.Init()

			// Service Without annotation
			_ = mockCtlr.processLBServices(svc1, false)
			Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Resource Config should be empty")

			svc1.Annotations = make(map[string]string)
			svc1.Annotations[LBServiceIPAMLabelAnnotation] = "test"

			svc1, _ = mockCtlr.kubeClient.CoreV1().Services(svc1.ObjectMeta.Namespace).UpdateStatus(context.TODO(), svc1, metav1.UpdateOptions{})

			_ = mockCtlr.processLBServices(svc1, false)
			Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Resource Config should be empty")

			_ = mockCtlr.createIPAMResource()
			ipamCR := mockCtlr.getIPAMCR()

			ipamCR.Spec.HostSpecs = []*ficV1.HostSpec{
				{
					IPAMLabel: "test",
					Host:      "",
					Key:       svc1.Namespace + "/" + svc1.Name + "_svc",
				},
			}

			ipamCR.Status.IPStatus = []*ficV1.IPSpec{
				{
					IPAMLabel: "test",
					Host:      "",
					IP:        "10.10.10.1",
					Key:       svc1.Namespace + "/" + svc1.Name + "_svc",
				},
			}
			ipamCR, _ = mockCtlr.ipamCli.Update(ipamCR)

			_ = mockCtlr.processLBServices(svc1, false)
			Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Invalid Resource Configs")

			_ = mockCtlr.processLBServices(svc1, true)
			Expect(len(mockCtlr.resources.ltmConfig[mockCtlr.Partition].ResourceMap)).To(Equal(0), "Invalid Resource Configs")
			Expect(len(svc1.Status.LoadBalancer.Ingress)).To(Equal(1))
		})

		It("Processing External DNS", func() {
			mockCtlr.resources.Init()
			DEFAULT_PARTITION = "default"
			mockCtlr.TeemData = &teem.TeemsData{
				ResourceType: teem.ResourceTypes{
					ExternalDNS: make(map[string]int),
				},
			}
			mockCtlr.Partition = "default"

			newEDNS := test.NewExternalDNS(
				"SampleEDNS",
				namespace,
				cisapiv1.ExternalDNSSpec{
					DomainName: "test.com",
					Pools: []cisapiv1.DNSPool{
						{
							DataServerName: "DataServer",
							Monitor: cisapiv1.Monitor{
								Type:     "http",
								Send:     "GET /health",
								Interval: 10,
								Timeout:  10,
							},
						},
					},
				})
			mockCtlr.processExternalDNS(newEDNS, false)
			gtmConfig := mockCtlr.resources.gtmConfig[DEFAULT_PARTITION].WideIPs
			Expect(len(gtmConfig)).To(Equal(1))
			Expect(len(gtmConfig["test.com"].Pools)).To(Equal(1))
			Expect(len(gtmConfig["test.com"].Pools[0].Members)).To(Equal(0))

			mockCtlr.resources.ltmConfig["default"] = &PartitionConfig{make(ResourceMap), 0}
			mockCtlr.resources.ltmConfig["default"].ResourceMap["SampleVS"] = &ResourceConfig{
				MetaData: metaData{
					hosts: []string{"test.com"},
				},
			}
			mockCtlr.processExternalDNS(newEDNS, false)
			gtmConfig = mockCtlr.resources.gtmConfig[DEFAULT_PARTITION].WideIPs
			Expect(len(gtmConfig)).To(Equal(1))
			Expect(len(gtmConfig["test.com"].Pools)).To(Equal(1))
			Expect(len(gtmConfig["test.com"].Pools[0].Members)).To(Equal(1))

			mockCtlr.processExternalDNS(newEDNS, true)
			gtmConfig = mockCtlr.resources.gtmConfig[DEFAULT_PARTITION].WideIPs
			Expect(len(gtmConfig)).To(Equal(0))
		})

		It("Processing IngressLink", func() {
			// Creation of IngressLink
			fooPorts := []v1.ServicePort{
				{
					Port: 8080,
					Name: "port0",
				},
			}
			foo := test.NewService("foo", "1", namespace, v1.ServiceTypeClusterIP, fooPorts)
			label1 := make(map[string]string)
			label1["app"] = "ingresslink"
			foo.ObjectMeta.Labels = label1
			var (
				selctor = &metav1.LabelSelector{
					MatchLabels: label1,
				}
			)
			var iRules []string
			IngressLink1 := test.NewIngressLink("ingresslink1", namespace, "1",
				cisapiv1.IngressLinkSpec{
					VirtualServerAddress: "1.2.3.4",
					Selector:             selctor,
					IRules:               iRules,
				})
			_ = mockCtlr.crInformers["default"].ilInformer.GetIndexer().Add(IngressLink1)
			mockCtlr.TeemData = &teem.TeemsData{
				ResourceType: teem.ResourceTypes{
					IngressLink: make(map[string]int),
				},
			}
			_ = mockCtlr.comInformers["default"].svcInformer.GetIndexer().Add(foo)
			err := mockCtlr.processIngressLink(IngressLink1, false)
			Expect(err).To(BeNil(), "Failed to process IngressLink while creation")
			Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Invalid LTM Config")
			Expect(mockCtlr.resources.ltmConfig).Should(HaveKey(mockCtlr.Partition),
				"Invalid LTM Config")
			Expect(len(mockCtlr.resources.ltmConfig[mockCtlr.Partition].ResourceMap)).To(Equal(1),
				"Invalid Resource Config")

			// Deletion of IngressLink
			err = mockCtlr.processIngressLink(IngressLink1, true)
			Expect(err).To(BeNil(), "Failed to process IngressLink while deletion")
			Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Invalid LTM Config")
			Expect(mockCtlr.resources.ltmConfig).Should(HaveKey(mockCtlr.Partition), "Invalid LTM Config")
			Expect(len(mockCtlr.resources.ltmConfig[mockCtlr.Partition].ResourceMap)).To(Equal(0),
				"Invalid Resource Config")

		})
	})

	It("get node port", func() {
		svc1.Spec.Ports[0].NodePort = 30000
		np := getNodeport(svc1, 80)
		Expect(int(np)).To(Equal(30000))
	})

	Describe("Test NodeportLocal", func() {
		var nplsvc *v1.Service
		var selectors map[string]string
		BeforeEach(func() {
			mockCtlr.PoolMemberType = NodePortLocal
			selectors = make(map[string]string)
			selectors["app"] = "npl"
			nplsvc = test.NewServicewithselectors(
				"svcnpl",
				"1",
				namespace,
				selectors,
				v1.ServiceTypeClusterIP,
				[]v1.ServicePort{
					{
						Port: 8080,
						Name: "port0",
					},
				},
			)
			ann := make(map[string]string)
			ann[NPLSvcAnnotation] = "true"
			nplsvc.Annotations = ann
			mockCtlr.comInformers["default"] = mockCtlr.newNamespacedCommonResourceInformer("default")
		})
		It("NodePortLocal", func() {
			pod1 := test.NewPod("pod1", namespace, 8080, selectors)
			ann := make(map[string]string)
			ann[NPLPodAnnotation] = "[{\"podPort\":8080,\"nodeIP\":\"10.10.10.1\",\"nodePort\":40000}]"
			pod1.Annotations = ann
			pod2 := test.NewPod("pod2", namespace, 8080, selectors)
			ann2 := make(map[string]string)
			ann2[NPLPodAnnotation] = "[{\"podPort\":8080,\"nodeIP\":\"10.10.10.1\",\"nodePort\":40001}]"
			pod2.Annotations = ann2
			mockCtlr.resources.Init()
			mockCtlr.processPod(pod1, false)
			mockCtlr.processPod(pod2, false)
			var val1 NPLAnnoations
			var val2 NPLAnnoations
			json.Unmarshal([]byte(pod1.Annotations[NPLPodAnnotation]), &val1)
			json.Unmarshal([]byte(pod2.Annotations[NPLPodAnnotation]), &val2)
			//verify npl store populated
			Expect(mockCtlr.resources.nplStore[namespace+"/"+pod1.Name]).To(Equal(val1))
			Expect(mockCtlr.resources.nplStore[namespace+"/"+pod2.Name]).To(Equal(val2))
			//verify selector match on pod
			Expect(mockCtlr.matchSvcSelectorPodLabels(map[string]string{}, pod1.Labels)).To(Equal(false))
			Expect(mockCtlr.matchSvcSelectorPodLabels(selectors, map[string]string{})).To(Equal(false))
			Expect(mockCtlr.matchSvcSelectorPodLabels(selectors, pod1.Labels)).To(Equal(true))
			Expect(mockCtlr.checkCoreserviceLabels(pod1.Labels)).To(Equal(false))
			var pods []*v1.Pod
			pods = append(pods, pod1, pod2)
			//Verify endpoints
			members := []PoolMember{
				{
					Address: "10.10.10.1",
					Port:    40000,
					Session: "user-enabled",
				},
				{
					Address: "10.10.10.1",
					Port:    40001,
					Session: "user-enabled",
				},
			}
			mems := mockCtlr.getEndpointsForNPL(intstr.FromInt(8080), pods)
			Expect(mems).To(Equal(members))
			mockCtlr.processPod(pod1, true)
			Expect(mockCtlr.resources.nplStore[namespace+"/"+pod1.Name]).To(BeNil())
			ann[NPLPodAnnotation] = "[{\"podPort\",\"nodeIP\":\"10.10.10.1\",\"nodePort\":40000}]"
			pod1.Annotations = ann
			mockCtlr.processPod(pod1, false)
			Expect(mockCtlr.resources.nplStore[namespace+"/"+pod1.Name]).To(BeNil())
			Expect(mockCtlr.GetPodsForService("test", "svc", true)).To(BeNil())
			Expect(mockCtlr.GetPodsForService("default", "svc", true)).To(BeNil())
			fooPorts := []v1.ServicePort{{Port: 80, NodePort: 30001},
				{Port: 8080, NodePort: 38001},
				{Port: 9090, NodePort: 39001}}
			svc := test.NewService("svc", "1", "default", "ClusterIP", fooPorts)
			mockCtlr.addService(svc)
			Expect(mockCtlr.GetPodsForService("default", "svc", true)).To(BeNil())
			svc.Annotations = map[string]string{"nodeportlocal.antrea.io/enabled": "enabled"}
			mockCtlr.updateService(svc)
			Expect(mockCtlr.GetPodsForService("default", "svc", true)).To(BeNil())
			labels := make(map[string]string)
			labels["app"] = "UpdatePoolHealthMonitors"
			svc.Spec.Selector = labels
			mockCtlr.updateService(svc)
			Expect(mockCtlr.GetPodsForService("default", "svc", true)).To(BeNil())
			pod1.Labels = labels
			mockCtlr.addPod(pod1)
			mockCtlr.kubeClient.CoreV1().Pods("default").Create(context.TODO(), pod1, metav1.CreateOptions{})
			Expect(mockCtlr.GetPodsForService("default", "svc", true)).ToNot(BeNil())
			Expect(mockCtlr.GetService("test", "svc")).To(BeNil())
			Expect(mockCtlr.GetService("default", "svc1")).To(BeNil())
			Expect(mockCtlr.GetService("default", "svc")).ToNot(BeNil())
			Expect(getNodeport(svc, 81)).To(BeEquivalentTo(0))
		})

		Describe("Processing Service of type LB with policy", func() {
			It("Processing ServiceTypeLoadBalancer with Policy", func() {
				//Policy CR
				namespace = "default"
				plc := test.NewPolicy("plc1", namespace,
					cisapiv1.PolicySpec{
						Profiles: cisapiv1.ProfileSpec{
							TCP: cisapiv1.ProfileTCP{
								Client: "/Common/f5-tcp-wan",
							},
							ProfileL4:          "/Common/security-fastL4",
							PersistenceProfile: "source-address",
							LogProfiles:        []string{"/Common/local-dos"},
						},
					},
				)
				mockCtlr.Agent = &Agent{
					PostManager: &PostManager{
						PostParams: PostParams{
							BIGIPURL: "10.10.10.1",
						},
					},
				}
				mockCtlr.Partition = namespace
				mockCtlr.ipamCli = ipammachinery.NewFakeIPAMClient(nil, nil, nil)
				mockCtlr.eventNotifier = apm.NewEventNotifier(nil)

				svc1.Spec.Type = v1.ServiceTypeLoadBalancer

				mockCtlr.resources.Init()

				// Service Without annotation
				_ = mockCtlr.processLBServices(svc1, false)
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0),
					"Resource Config should be empty")

				svc1.Annotations = make(map[string]string)
				svc1.Annotations[LBServiceIPAMLabelAnnotation] = "test"
				svc1.Annotations[LBServicePolicyNameAnnotation] = "plc1"

				svc1, _ = mockCtlr.kubeClient.CoreV1().Services(svc1.ObjectMeta.Namespace).UpdateStatus(
					context.TODO(), svc1, metav1.UpdateOptions{})

				_ = mockCtlr.createIPAMResource()
				ipamCR := mockCtlr.getIPAMCR()

				ipamCR.Spec.HostSpecs = []*ficV1.HostSpec{
					{
						IPAMLabel: "test",
						Host:      "",
						Key:       svc1.Namespace + "/" + svc1.Name + "_svc",
					},
				}

				ipamCR.Status.IPStatus = []*ficV1.IPSpec{
					{
						IPAMLabel: "test",
						Host:      "",
						IP:        "10.10.10.1",
						Key:       svc1.Namespace + "/" + svc1.Name + "_svc",
					},
				}
				ipamCR, _ = mockCtlr.ipamCli.Update(ipamCR)

				// Policy CRD not found
				_ = mockCtlr.processLBServices(svc1, false)
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0),
					"Resource Config should be empty")

				mockCtlr.comInformers[namespace].plcInformer = cisinfv1.NewFilteredPolicyInformer(
					mockCtlr.kubeCRClient,
					namespace,
					0,
					cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
					func(options *metav1.ListOptions) {
						options.LabelSelector = mockCtlr.nativeResourceSelector.String()
					},
				)
				_ = mockCtlr.comInformers[namespace].plcInformer.GetStore().Add(plc)

				// Policy CRD exists
				_ = mockCtlr.processLBServices(svc1, false)
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Invalid Resource Configs")
				rsname := "vs_lb_svc_default_svc1_10_10_10_1_80"
				Expect(mockCtlr.resources.ltmConfig[namespace].ResourceMap[rsname].Virtual.SNAT).To(Equal(DEFAULT_SNAT),
					"Invalid Resource Configs")
				Expect(mockCtlr.resources.ltmConfig[namespace].ResourceMap[rsname].Virtual.PersistenceProfile).To(Equal(
					plc.Spec.Profiles.PersistenceProfile), "Invalid Resource Configs")
				Expect(mockCtlr.resources.ltmConfig[namespace].ResourceMap[rsname].Virtual.ProfileL4).To(Equal(
					plc.Spec.Profiles.ProfileL4), "Invalid Resource Configs")
				Expect(len(mockCtlr.resources.ltmConfig[namespace].ResourceMap[rsname].Virtual.LogProfiles)).To(
					Equal(1), "Invalid Resource Configs")

				// SNAT set to SNAT pool name
				plc.Spec.SNAT = "Common/test"
				_ = mockCtlr.comInformers[namespace].plcInformer.GetStore().Update(plc)
				_ = mockCtlr.processLBServices(svc1, false)
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Invalid Resource Configs")
				Expect(mockCtlr.resources.ltmConfig[namespace].ResourceMap[rsname].Virtual.SNAT).To(Equal(plc.Spec.SNAT),
					"Invalid Resource Configs")

				// SNAT set to none
				plc.Spec.SNAT = "none"
				_ = mockCtlr.comInformers[namespace].plcInformer.GetStore().Update(plc)
				_ = mockCtlr.processLBServices(svc1, false)
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Invalid Resource Configs")
				Expect(mockCtlr.resources.ltmConfig[namespace].ResourceMap[rsname].Virtual.SNAT).To(Equal(plc.Spec.SNAT),
					"Invalid Resource Configs")

			})
		})
	})

	Describe("Getting Associated virtuals", func() {
		var vrt1 *cisapiv1.VirtualServer
		var vrt2 *cisapiv1.VirtualServer
		BeforeEach(func() {
			vrt1 = test.NewVirtualServer(
				"vrt1",
				namespace,
				cisapiv1.VirtualServerSpec{
					TLSProfileName: "tls-profile-1",
				},
			)
			vrt2 = test.NewVirtualServer(
				"vrt2",
				namespace,
				cisapiv1.VirtualServerSpec{
					TLSProfileName: "tls-profile-2",
				},
			)
		})
		It("Correctly skips adding the virtuals to associated virtuals if ports are not common", func() {
			// Virtuals with common HTTP AND HTTPS ports
			Expect(skipVirtual(vrt1, vrt2)).To(Equal(false), "Should not skip adding it to "+
				"associated virtuals")
			// Virtuals without any common HTTP AND HTTPS ports
			vrt2.Spec.VirtualServerHTTPSPort = 8443
			vrt2.Spec.VirtualServerHTTPPort = 8080
			Expect(skipVirtual(vrt1, vrt2)).To(Equal(true), "Should skip adding it to "+
				"associated virtuals")
			// Secured virtuals with common HTTPS ports
			vrt1.Spec.VirtualServerHTTPSPort = 8443
			Expect(skipVirtual(vrt1, vrt2)).To(Equal(false), "Should not skip adding it to "+
				"associated virtuals")
			// Virtuals with common HTTPS ports(default 443) but one of them is unsecured vs
			vrt2.Spec.VirtualServerHTTPSPort = 0
			vrt1.Spec.VirtualServerHTTPSPort = 0
			vrt2.Spec.TLSProfileName = ""
			Expect(skipVirtual(vrt1, vrt2)).To(Equal(true), "Should skip adding it to "+
				"associated virtuals")
			// Secured virtuals with common HTTP ports, but HTTPTraffic is not allowed
			vrt2.Spec.VirtualServerHTTPPort = 0
			vrt2.Spec.VirtualServerHTTPSPort = 8443
			vrt2.Spec.TLSProfileName = "tls-profile-2"
			Expect(skipVirtual(vrt1, vrt2)).To(Equal(true), "Should skip adding it to "+
				"associated virtuals")
			// Both secured virtuals with common HTTP ports, and handle HTTPTraffic
			vrt1.Spec.HTTPTraffic = TLSAllowInsecure
			vrt2.Spec.HTTPTraffic = TLSRedirectInsecure
			Expect(skipVirtual(vrt1, vrt2)).To(Equal(false), "Should not skip adding it to "+
				"associated virtuals")
			// Both secured virtuals with common HTTP ports, and one of them doesn't handle HTTPTraffic
			vrt1.Spec.HTTPTraffic = "none"
			vrt2.Spec.HTTPTraffic = TLSRedirectInsecure
			Expect(skipVirtual(vrt1, vrt2)).To(Equal(true), "Should skip adding it to "+
				"associated virtuals")
			// One secured and one unsecured vs with common HTTP ports, and the secured one doesn't handle HTTPTraffic
			vrt2.Spec.TLSProfileName = ""
			Expect(skipVirtual(vrt1, vrt2)).To(Equal(true), "Should skip adding it to "+
				"associated virtuals")
			// One secured and one unsecured vs with common HTTP ports, and the secured one handles HTTPTraffic
			vrt1.Spec.HTTPTraffic = TLSAllowInsecure
			Expect(skipVirtual(vrt1, vrt2)).To(Equal(false), "Should not skip adding it to "+
				"associated virtuals")
			// Both unsecured virtuals with common HTTP ports
			vrt1.Spec.TLSProfileName = ""
			Expect(skipVirtual(vrt1, vrt2)).To(Equal(false), "Should not skip adding it to "+
				"associated virtuals")
		})
		It("Verifies whether correct effective HTTPS port is evaluated for the virtual server", func() {
			Expect(getEffectiveHTTPSPort(vrt1)).To(Equal(DEFAULT_HTTPS_PORT), "Incorrect HTTPS port "+
				"value evaluated")
			vrt1.Spec.VirtualServerHTTPSPort = 8443
			Expect(getEffectiveHTTPSPort(vrt1)).To(Equal(vrt1.Spec.VirtualServerHTTPSPort), "Incorrect "+
				" HTTPS port value evaluated")
			vrt1.Spec.VirtualServerHTTPSPort = DEFAULT_HTTPS_PORT
			Expect(getEffectiveHTTPSPort(vrt1)).To(Equal(DEFAULT_HTTPS_PORT), "Incorrect HTTPS "+
				"port value evaluated")
		})
		It("Verifies whether correct effective HTTP port is evaluated for the virtual server", func() {
			Expect(getEffectiveHTTPPort(vrt1)).To(Equal(DEFAULT_HTTP_PORT), "Incorrect HTTP port value "+
				"evaluated")
			vrt1.Spec.VirtualServerHTTPPort = 8080
			Expect(getEffectiveHTTPPort(vrt1)).To(Equal(vrt1.Spec.VirtualServerHTTPPort), "Incorrect "+
				"HTTP port value evaluated")
			vrt1.Spec.VirtualServerHTTPPort = DEFAULT_HTTP_PORT
			Expect(getEffectiveHTTPPort(vrt1)).To(Equal(DEFAULT_HTTP_PORT), "Incorrect HTTP "+
				"port value evaluated")
		})
	})

	Describe("Deletion of virtuals", func() {
		var vrts []*cisapiv1.VirtualServer
		var vrt2 *cisapiv1.VirtualServer
		BeforeEach(func() {
			vrts = []*cisapiv1.VirtualServer{test.NewVirtualServer(
				"vrt1",
				namespace,
				cisapiv1.VirtualServerSpec{
					TLSProfileName: "tls-profile-1",
					HTTPTraffic:    TLSAllowInsecure,
				},
			)}
			vrt2 = test.NewVirtualServer(
				"vrt2",
				namespace,
				cisapiv1.VirtualServerSpec{
					TLSProfileName:         "tls-profile-2",
					HTTPTraffic:            TLSAllowInsecure,
					VirtualServerHTTPSPort: 8443,
				},
			)
		})
		It("Verifies whether any of the associated virtuals handle HTTP traffic", func() {
			// Check doVSHandleHTTP when associated virtuals handle HTTP traffic
			Expect(doVSHandleHTTP(vrts, vrt2)).To(Equal(true), "Invalid value")
			// Check doVSHandleHTTP when associated virtuals don't handle HTTP traffic
			vrts[0].Spec.HTTPTraffic = ""
			Expect(doVSHandleHTTP(vrts, vrt2)).To(Equal(false), "Invalid value")
			// Check doVSHandleHTTP when associated unsecured virtual uses the same port that the current virtual does
			vrts[0].Spec.TLSProfileName = ""
			Expect(doVSHandleHTTP(vrts, vrt2)).To(Equal(true), "Invalid value")
			// Check doVSHandleHTTP when associated unsecured virtual uses a different port
			vrts[0].Spec.VirtualServerHTTPPort = 8080
			Expect(doVSHandleHTTP(vrts, vrt2)).To(Equal(false), "Invalid value")
		})
		It("Verifies whether any of the associated virtuals uses the same HTTPS port", func() {
			// Check when associated secured virtuals use same HTTPS port
			vrts[0].Spec.VirtualServerHTTPSPort = 8443
			Expect(doVSUseSameHTTPSPort(vrts, vrt2)).To(Equal(true), "Invalid value")
			// Check when none of the associated secured virtuals uses same HTTPS port
			vrts[0].Spec.VirtualServerHTTPSPort = 443
			Expect(doVSUseSameHTTPSPort(vrts, vrt2)).To(Equal(false), "Invalid value")
			// Check when associated virtuals has an unsecured virtual
			vrts[0].Spec.TLSProfileName = ""
			Expect(doVSUseSameHTTPSPort(vrts, vrt2)).To(Equal(false), "Invalid value")

		})
	})
	Describe("Update Pool Members for nodeport", func() {
		BeforeEach(func() {
			mockCtlr.crInformers = make(map[string]*CRInformer)
			mockCtlr.comInformers = make(map[string]*CommonInformer)
			mockCtlr.crInformers["default"] = &CRInformer{}
			mockCtlr.comInformers["default"] = &CommonInformer{}
			mockCtlr.resources.poolMemCache = make(map[string]poolMembersInfo)
			mockCtlr.resources.ltmConfig = LTMConfig{}
			mockCtlr.oldNodes = []Node{{Name: "node-1", Addr: "10.10.10.1"}, {Name: "node-2", Addr: "10.10.10.2"}}
		})
		It("verify pool member update", func() {
			memberMap := make(map[portRef][]PoolMember)
			var nodePort int32 = 30000
			members := []PoolMember{
				{
					Address: "10.10.10.1",
					Port:    nodePort,
					Session: "user-enabled",
				},
				{
					Address: "10.10.10.2",
					Port:    nodePort,
					Session: "user-enabled",
				},
			}
			memberMap[portRef{name: "https", port: 443}] = members
			mockCtlr.resources.poolMemCache["default/svc-1"] = poolMembersInfo{
				svcType:   "Nodeport",
				portSpec:  []v1.ServicePort{{Name: "https", Port: 443, NodePort: 32443, TargetPort: intstr.FromInt(443), Protocol: "TCP"}},
				memberMap: memberMap,
			}
			pool := Pool{ServiceNamespace: "default",
				ServiceName: "svc-1",
				ServicePort: intstr.FromInt(443)}
			pool2 := Pool{ServiceNamespace: "default",
				ServiceName: "svc-2",
				ServicePort: intstr.FromInt(443),
				Members:     members}
			rsCfg := &ResourceConfig{Pools: []Pool{pool, {}}}
			rsCfg2 := &ResourceConfig{Pools: []Pool{pool2}}
			mockCtlr.updatePoolMembersForNodePort(rsCfg2, "default")
			Expect(len(rsCfg2.Pools[0].Members)).To(Equal(0), "Members should be updated to zero")
			mockCtlr.updatePoolMembersForNodePort(rsCfg, "test")
			Expect(len(rsCfg.Pools[0].Members)).To(Equal(0), "Members should not be updated as namespace is not being watched")
			mockCtlr.updatePoolMembersForNodePort(rsCfg, "default")
			Expect(len(rsCfg.Pools[0].Members)).To(Equal(2), "Members should not be updated")
			mockCtlr.oldNodes = append(mockCtlr.oldNodes, Node{Name: "node-3", Addr: "10.10.10.3"})
			mockCtlr.updatePoolMembersForNodePort(rsCfg, "default")
			Expect(len(rsCfg.Pools[0].Members)).To(Equal(3), "Members should be increased")
			mockCtlr.PoolMemberType = NodePort
			mockCtlr.updateSvcDepResources("test-resource", rsCfg)
			mockCtlr.resources.ltmConfig["test"] = &PartitionConfig{ResourceMap: ResourceMap{}}
			mockCtlr.resources.setResourceConfig("test", "test-resource", rsCfg)
			rsCfgCopy := mockCtlr.getVirtualServer("test", "test-resource")
			Expect(rsCfgCopy).ToNot(BeNil())
			Expect(len(rsCfgCopy.Pools[0].Members)).To(Equal(3), "There should be three pool members")
			mockCtlr.oldNodes = append(mockCtlr.oldNodes[:1], mockCtlr.oldNodes[2:]...)
			svc := test.NewService("svc-1", "1", "default", "NodePort", []v1.ServicePort{})
			mockCtlr.updatePoolMembersForVirtuals(svc)
			rsCfgCopy = mockCtlr.getVirtualServer("test", "test-resource")
			Expect(rsCfgCopy).ToNot(BeNil())
			Expect(len(rsCfgCopy.Pools[0].Members)).To(Equal(2), "Pool members should be updated to 2")
		})
	})
	Describe("Processing Custom Resources", func() {
		var mockPM *mockPostManager
		var policy *cisapiv1.Policy
		BeforeEach(func() {
			mockCtlr.mode = CustomResourceMode
			mockCtlr.namespaces = make(map[string]bool)
			mockCtlr.namespaces["default"] = true
			mockCtlr.kubeCRClient = crdfake.NewSimpleClientset()
			mockCtlr.kubeClient = k8sfake.NewSimpleClientset()
			mockCtlr.crInformers = make(map[string]*CRInformer)
			mockCtlr.nsInformers = make(map[string]*NSInformer)
			mockCtlr.comInformers = make(map[string]*CommonInformer)
			mockCtlr.customResourceSelector, _ = createLabelSelector(DefaultCustomResourceLabel)
			mockCtlr.resourceQueue = workqueue.NewNamedRateLimitingQueue(
				workqueue.DefaultControllerRateLimiter(), "custom-resource-controller")
			mockCtlr.resources = NewResourceStore()
			mockCtlr.comInformers["default"] = mockCtlr.newNamespacedCommonResourceInformer("default")

			mockCtlr.TeemData = &teem.TeemsData{
				ResourceType: teem.ResourceTypes{
					RouteGroups:     make(map[string]int),
					NativeRoutes:    make(map[string]int),
					ExternalDNS:     make(map[string]int),
					IngressLink:     make(map[string]int),
					VirtualServer:   make(map[string]int),
					TransportServer: make(map[string]int),
				},
			}

			mockCtlr.requestQueue = &requestQueue{sync.Mutex{}, list.New()}
			err := mockCtlr.addNamespacedInformers(namespace, false)
			Expect(err).To(BeNil(), "Informers Creation Failed")

			mockCtlr.Agent = &Agent{
				postChan:            make(chan ResourceConfigRequest, 1),
				cachedTenantDeclMap: make(map[string]as3Tenant),
				respChan:            make(chan resourceStatusMeta, 1),
				retryTenantDeclMap:  make(map[string]*tenantParams),
			}

			mockPM = newMockPostManger()
			mockPM.BIGIPURL = "bigip.com"
			mockPM.BIGIPUsername = "user"
			mockPM.BIGIPPassword = "pswd"
			mockPM.tenantResponseMap = make(map[string]tenantResponse)
			mockPM.LogResponse = true
			//					mockPM.AS3PostDelay =
			mockPM.setupBIGIPRESTClient()
			tnt := "test"
			mockPM.setResponses([]responceCtx{{
				tenant: tnt,
				status: http.StatusOK,
				body:   "",
			}}, http.MethodPost)
			mockPM.firstPost = false
			mockCtlr.Agent.PostManager = mockPM.PostManager

			mockCtlr.ipamCli = ipammachinery.NewFakeIPAMClient(nil, nil, nil)
			_ = mockCtlr.createIPAMResource()

			policy = &cisapiv1.Policy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "policy",
					Namespace: "default",
				},
				Spec: cisapiv1.PolicySpec{
					SNAT: "auto",
					L7Policies: cisapiv1.L7PolicySpec{
						WAF: "/Common/WAF_Policy",
					},
					L3Policies: cisapiv1.L3PolicySpec{
						FirewallPolicy: "/Common/AFM_Policy",
						DOS:            "/Common/dos",
						BotDefense:     "/Common/bot-defense",
						AllowSourceRange: []string{
							"1.1.1.0/24",
							"2.2.2.0/24",
						},
						AllowVlans: []string{
							" /Common/external",
						},
					},
					Profiles: cisapiv1.ProfileSpec{
						TCP: cisapiv1.ProfileTCP{
							Client: "/Common/f5-tcp-lan",
							Server: "/Common/f5-tcp-wan",
						},
						HTTP:  "/Common/http",
						HTTP2: "/Common/http2",
						LogProfiles: []string{
							"/Common/Log all requests", "/Common/local-dos"},
						ProfileL4:        " /Common/security-fastL4",
						ProfileMultiplex: "/Common/oneconnect",
						UDP:              "/Common/udp",
					},
				},
			}
		})
		AfterEach(func() {
			mockCtlr.resourceQueue.ShutDown()
		})

		Describe("Processing Virtual Server", func() {
			AfterEach(func() {
				mockCtlr.resourceQueue.ShutDown()
			})

			var vs *cisapiv1.VirtualServer
			var tlsProf *cisapiv1.TLSProfile
			var secret *v1.Secret
			var tlsSecretProf *cisapiv1.TLSProfile
			var fooEndpts *v1.Endpoints
			var fooPorts []v1.ServicePort

			BeforeEach(func() {
				//Add Virtual Server
				vs = test.NewVirtualServer(
					"SampleVS",
					namespace,
					cisapiv1.VirtualServerSpec{
						Host:           "test.com",
						PolicyName:     "policy",
						TLSProfileName: "sampleTLS",
						Pools: []cisapiv1.Pool{
							{
								Path:    "/foo",
								Service: "svc1",
								Monitor: cisapiv1.Monitor{
									Type:     "http",
									Send:     "GET /health",
									Interval: 15,
									Timeout:  10,
								},
								Rewrite:     "/bar",
								Balance:     "fastest-node",
								ServicePort: 80,
							},
							{
								Path:    "/",
								Service: "svc2",
								Monitor: cisapiv1.Monitor{
									Type:     "http",
									Send:     "GET /health",
									Interval: 15,
									Timeout:  10,
								},
								Rewrite:     "/bar1",
								Balance:     "fastest-node",
								ServicePort: 81,
							},
						},
						RewriteAppRoot:     "/home",
						AllowSourceRange:   []string{" 1.1.1.0/24", "2.2.2.0/24"},
						BotDefense:         "/Common/bot-defense",
						DOS:                "/Common/dos",
						WAF:                "/Common/WAF",
						IRules:             []string{"/Common/SampleIRule"},
						PersistenceProfile: "source-address",
						AllowVLANs:         []string{"/Common/devtraffic"},
						Profiles: cisapiv1.ProfileSpec{
							TCP: cisapiv1.ProfileTCP{
								Client: "/Common/f5-tcp-lan",
								Server: "/Common/f5-tcp-wan",
							},
							ProfileL4: "/Common/security-fastL4",
						},
					},
				)

				fooPorts = []v1.ServicePort{{Port: 80, NodePort: 30001},
					{Port: 8080, NodePort: 38001},
					{Port: 9090, NodePort: 39001}}
				fooIps := []string{"10.1.1.1"}

				fooEndpts = test.NewEndpoints(
					"svc1", "1", "node0", namespace, fooIps, []string{},
					convertSvcPortsToEndpointPorts(fooPorts))

				tlsProf = test.NewTLSProfile(
					"sampleTLS",
					namespace,
					cisapiv1.TLSProfileSpec{
						Hosts: []string{"test.com"},
						TLS: cisapiv1.TLS{
							Termination: TLSReencrypt,
							ClientSSL:   "clientssl",
							ServerSSL:   "serverssl",
							Reference:   BIGIP,
						},
					},
				)
				tlsSecretProf = test.NewTLSProfile(
					"sampleTLS",
					namespace,
					cisapiv1.TLSProfileSpec{
						Hosts: []string{"test.com"},
						TLS: cisapiv1.TLS{
							Termination: TLSEdge,
							ClientSSL:   "SampleSecret",
							Reference:   Secret,
						},
					},
				)

				secret = test.NewSecret(
					"SampleSecret",
					namespace,
					"-----BEGIN CERTIFICATE-----\nMIIC+DCCAeCgAwIBAgIQIBIcC6PuJQEHwwI0Hv5QmTANBgkqhkiG9w0BAQsFADAS\nMRAwDgYDVQQKEwdBY21lIENvMB4XDTIyMTIyMjA5MjE0OFoXDTIzMTIyMjA5MjE0\nOFowEjEQMA4GA1UEChMHQWNtZSBDbzCCASIwDQYJKoZIhvcNAQEBBQADggEPADCC\nAQoCggEBAN0NWXsUvGYBV9uo2Iuz3gnovyk3W7p8AA4I8eRUFaWV1EYaxFpsGmdN\nrQgdVJ6w+POSykbDuZynYJyBjC11dJmfTaXffLaUSrJfu+a0QaeWIpt+XxzO4SKQ\nunUSh5Z9w4P45G8VKF7E67wFVN0ni10FLAfBUjYVsQpPagpkH8OdnYCsymCzVSWi\nYETZZ+Hbaih9flRgBQOsoUyNBSkCdJ2wEkZ/0p9+tYwZp1Xvp/Neu3TTsezpu7lE\nbTp0RLQNqfLHWiMV9BSAQRbXAvtvky3J42iy+ec24JyQPtiD85u8Pp/+ssV0ZL9l\nc5KoDEuAvf4NPFWu270gYyQljKcTbB8CAwEAAaNKMEgwDgYDVR0PAQH/BAQDAgWg\nMBMGA1UdJQQMMAoGCCsGAQUFBwMBMAwGA1UdEwEB/wQCMAAwEwYDVR0RBAwwCoII\ndGVzdC5jb20wDQYJKoZIhvcNAQELBQADggEBAI9VUdpVmfx+WUEejREa+plEjCIV\ns+d7v66ddyU4B+Zer1y4RgoWaVq5pywPPjBNJuz6NfwSvBCmuMUd1LUoF5tQFkqb\nVa85Aq6ODbwIMoQ53kTG9vLbT78qESrbukaW9v+axdD9/DIXZJtdwvLvHAVpelRi\n7z48Lxk1GTe7dM3ixKQrU4hz656kH3kXSnD79metOkJA6BAXsqL2XonIhNkCkQVV\n38IHDNkzk228d97ebLu+EhLlkjFgFQEnXusK1amrGJrRDli72pY01yxzGI1caKG5\nN6I8MEIqYI/POwbYWENqONF22pzw/OIs4T1a3jjUqEFugnELcTtx/xRLmOI=\n-----END CERTIFICATE-----\n",
					"-----BEGIN PRIVATE KEY-----\nMIIEvAIBADANBgkqhkiG9w0BAQEFAASCBKYwggSiAgEAAoIBAQDdDVl7FLxmAVfb\nqNiLs94J6L8pN1u6fAAOCPHkVBWlldRGGsRabBpnTa0IHVSesPjzkspGw7mcp2Cc\ngYwtdXSZn02l33y2lEqyX7vmtEGnliKbfl8czuEikLp1EoeWfcOD+ORvFShexOu8\nBVTdJ4tdBSwHwVI2FbEKT2oKZB/DnZ2ArMpgs1UlomBE2Wfh22oofX5UYAUDrKFM\njQUpAnSdsBJGf9KffrWMGadV76fzXrt007Hs6bu5RG06dES0Danyx1ojFfQUgEEW\n1wL7b5MtyeNosvnnNuCckD7Yg/ObvD6f/rLFdGS/ZXOSqAxLgL3+DTxVrtu9IGMk\nJYynE2wfAgMBAAECggEAf8l91vcvylAweB1twaUjUNsp1yvXbUDNz09Adtxc/zJU\nWoqSxCsGQH3Y7331Mx/fav+Ky8nN/U+NPCxv2r+xvjUncCJ4OBwV6nQJbd76rWTP\ncNBnL4IxCAheodsqYsclRZ+WftjeU5rHJBR48Lgxin6462rImdeEVw99n7At5Kig\nGZmGNXnk6jgvoNU1YJZxSMWQQwKtrfJxXry5a90SfjiviGseuBPsgbrMxEPaeqlQ\nGAMi4nIVRmijL56vbbuuudZm+6dpOnbGzzF6J4M5Nrfr/qJF7ClwXjcMeb6lESIo\n5pmGl3QwSGQYeflFexP3ydvQdUwN5rLbtCexPC2CsQKBgQDxLPn8pIU7WuFiTuOp\n1o7/25v7ijPydIRBjjVeA7E7+mbq9FllkT4CW+HtP7zCCjdScuXhKjuPRrST4fsZ\nZex2nUYfc586s/W95b4QMKtXcJd1MMMWOK2/ZGN/6L5zLPupDrhyWHw91biFZG8h\nSFgn7G2zS/+09gJTglpdj3gClQKBgQDqo7f+kZiXGFvP4kcOWNgnOJOpdqzG/zeD\nuVP2Y6Q8mi7GhkiYhdlrl6Ibh9X0qjFMKMKy827jbUPSGaj5tIT8iXyFT4KVaqZQ\n7r2cMyCqbznKfWlyMyspaVEDa910+VwC2hYQvahTQzfdQqFp6JfiLqCdQtiNDGLf\nbvUOHk4a4wKBgHDLo0NowrMm5wBuewXExm6djE9RrMf5fJ2YYBdPTMYLb7T1gRYC\nnujFhl3KkIKD+qnB+QedE+wHmo8Lgr+3LqevGMu+7LqszgL5fzHdQVWM4Bk8LBGp\ngoFf9zUsal49rJm9u8Am6DyXR0yD04HSbwCFEC1qHvbIk//wmEjnv64dAoGANBbW\nYPBHlLt2nmbYaWn1ync36LYM0zyTQW3iIt+p9T4xRidHdHy6cLU/6qa0K9Wgjgy6\ndGmwY1K9bKX/qjeWEk4fU6T8E1mSxILLmyKKjOuWQ8qlnxGW8mGL95t5lV9KOuPZ\nZCwGcz2H6FnDZbSaCz9YrrDJTD7EsF98jX7SzgsCgYBQv5yi7aGxH6OcrAJPQH4v\n1fZo7mFbqp0WoUMpwuWKNOHZuZoF0EU/bllMZT7AipxVhso+hUC+rDEO7H36TAyc\nTUJbdxtlIC1JmJTmeBOWh3i3Htu8A97DLUNTqNikNyKyGWjy7eC0ncG3+CGG91wA\nky9KxzxszaIez6kIUCY7xQ==\n-----END PRIVATE KEY-----\n",
				)
			})

			It("Virtual Server with Virtual Address", func() {

				crInf := mockCtlr.newNamespacedCustomResourceInformer(namespace)
				nrInf := mockCtlr.newNamespacedNativeResourceInformer(namespace)
				crInf.start()
				nrInf.start()

				mockCtlr.addEndpoints(fooEndpts)
				mockCtlr.processResources()

				svc := test.NewService("svc1", "1", namespace, "NodePort", fooPorts)
				mockCtlr.addService(svc)
				mockCtlr.processResources()

				mockCtlr.kubeClient.CoreV1().Services("default").Create(context.TODO(), svc, metav1.CreateOptions{})
				mockCtlr.setInitialServiceCount()
				mockCtlr.migrateIPAM()

				mockCtlr.addVirtualServer(vs)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid Virtual Server")

				vs.Spec.VirtualServerAddress = "10.8.0.1"
				mockCtlr.addVirtualServer(vs)
				mockCtlr.processResources()

				// Policy and TLSProfile missing
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid Virtual Server")

				mockCtlr.addPolicy(policy)
				mockCtlr.processResources()

				mockCtlr.addTLSProfile(tlsSecretProf)
				mockCtlr.processResources()

				mockCtlr.addSecret(secret)
				mockCtlr.processResources()

				mockCtlr.kubeClient.CoreV1().Secrets("default").Create(context.TODO(), secret, metav1.CreateOptions{})
				mockCtlr.addVirtualServer(vs)
				mockCtlr.processResources()
				// Should process VS now
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Virtual Server not Processed")

				mockCtlr.deleteService(svc)
				mockCtlr.processResources()

				// Update service
				svc.Spec.Ports[0].NodePort = 30002
				Expect(fetchPortString(intstr.IntOrString{StrVal: strconv.Itoa(int(vs.Spec.Pools[0].ServicePort))})).To(BeEquivalentTo("80"))
				mockCtlr.addService(svc)
				mockCtlr.processResources()

				tlsSecretProf.Spec.TLS.ClientSSLs = []string{"SampleSecret"}
				mockCtlr.addTLSProfile(tlsSecretProf)
				mockCtlr.processResources()
				mockCtlr.addSecret(secret)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Virtual Server not processed")

				mockCtlr.deleteVirtualServer(vs)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Virtual Server not deleted")

				//check valid virtual server
				valid := mockCtlr.checkValidVirtualServer(vs)
				Expect(valid).To(BeFalse())

				mockCtlr.addVirtualServer(vs)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Virtual Server not processed")

				mockCtlr.deleteVirtualServer(vs)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Virtual Server not deleted")

				vs.Spec.VirtualServerAddress = ""
				mockCtlr.ipamCli = nil
				mockCtlr.addVirtualServer(vs)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid VS")
				vs.Spec.VirtualServerAddress = "10.8.0.1"
				// set HttpMrfRoutingEnabled to true
				vs.Spec.HttpMrfRoutingEnabled = true
				mockCtlr.addVirtualServer(vs)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Virtual Server not processed")
				rsname := "crd_10_8_0_1_443"
				Expect(mockCtlr.resources.ltmConfig[mockCtlr.Partition].ResourceMap[rsname].Virtual.HttpMrfRoutingEnabled).To(Equal(true), "HttpMrfRoutingEnabled not enabled on VS")

				// Validate the scenario. For now changing to 1
				mockCtlr.deletePolicy(policy)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Invalid VS")

				labels := make(map[string]string)
				labels["app"] = "test"
				ns := test.NewNamespace(
					"default",
					"1",
					labels,
				)
				mockCtlr.enqueueDeletedNamespace(ns)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Virtual Server not deleted")
				_, ok := mockCtlr.nsInformers[namespace]
				Expect(ok).To(Equal(false), "Namespace not deleted")

				// verify HTTPTraffic is not set for insecure virtual server
				vs.Spec.HTTPTraffic = TLSAllowInsecure
				vs.Spec.TLSProfileName = ""
				valid = mockCtlr.checkValidVirtualServer(vs)
				Expect(valid).To(BeFalse(), "HTTPTraffic not allowed to be set for insecure VS")
				vs.Spec.HTTPTraffic = TLSRedirectInsecure
				valid = mockCtlr.checkValidVirtualServer(vs)
				Expect(valid).To(BeFalse(), "HTTPTraffic not allowed to be set for insecure VS")

			})
			It("Virtual Server with IPAM", func() {
				go mockCtlr.Agent.agentWorker()
				go mockCtlr.Agent.retryWorker()

				go mockCtlr.responseHandler(mockCtlr.Agent.respChan)
				mockCtlr.addPolicy(policy)
				mockCtlr.processResources()

				mockCtlr.addTLSProfile(tlsProf)
				mockCtlr.processResources()

				mockCtlr.TeemData.ResourceType.IPAMVS = make(map[string]int)

				//	Add Service
				vs.Spec.IPAMLabel = "test"

				mockCtlr.addEndpoints(fooEndpts)
				mockCtlr.processResources()

				svc := test.NewService("svc1", "1", namespace, "NodePort", fooPorts)
				mockCtlr.addService(svc)
				mockCtlr.processResources()

				mockCtlr.addVirtualServer(vs)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid Virtual Server")

				var key, host string

				ipamCR := mockCtlr.getIPAMCR()
				host = "test.com"
				key = "default/test.com_host"

				ipamCR.Spec.HostSpecs = []*ficV1.HostSpec{
					{
						IPAMLabel: "test",
						Host:      "test.com",
					},
				}
				ipamCR, _ = mockCtlr.ipamCli.Update(ipamCR)
				newIpamCR := ipamCR.DeepCopy()

				newIpamCR.Status.IPStatus = []*ficV1.IPSpec{
					{
						IPAMLabel: "test",
						Host:      host,
						IP:        "10.10.10.1",
						Key:       key,
					},
				}
				newIpamCR, _ = mockCtlr.ipamCli.Update(newIpamCR)

				mockCtlr.enqueueUpdatedIPAM(ipamCR, newIpamCR)
				mockCtlr.processResources()

				_, status := mockCtlr.requestIP("test", host, key)
				Expect(status).To(Equal(Allocated), "Failed to fetch Allocated IP")
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "VS not Processed")

				mockCtlr.deleteVirtualServer(vs)
				mockCtlr.processResources()
				vs.Spec.VirtualServerAddress = "10.10.10.1"
				mockCtlr.addVirtualServer(vs)
				mockCtlr.processResources()

				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "VS not Processed")

				mockCtlr.deleteVirtualServer(vs)
				mockCtlr.processResources()

				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid Virtual Server")

				vs.Spec.HostGroup = "hg"
				vs.Spec.VirtualServerAddress = ""
				mockCtlr.addVirtualServer(vs)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid Virtual Server")

				key = "hg_hg"
				newIpamCR.Spec.HostSpecs = []*ficV1.HostSpec{
					{
						IPAMLabel: "test",
						Key:       key,
					},
				}
				newIpamCR, _ = mockCtlr.ipamCli.Update(newIpamCR)
				ipamCR = newIpamCR.DeepCopy()

				newIpamCR.Status.IPStatus = []*ficV1.IPSpec{
					{
						IPAMLabel: "test",
						//Host:      host,
						IP:  "10.10.10.1",
						Key: key,
					},
				}
				newIpamCR, _ = mockCtlr.ipamCli.Update(newIpamCR)

				mockCtlr.enqueueUpdatedIPAM(ipamCR, newIpamCR)
				mockCtlr.processResources()

				_, status = mockCtlr.requestIP("test", "", key)
				Expect(status).To(Equal(Allocated), "Failed to fetch Allocated IP")
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Virtual Server not processed")

				rscUpdateMeta := resourceStatusMeta{
					0,
					make(map[string]struct{}),
				}

				time.Sleep(10 * time.Millisecond)
				mockCtlr.Agent.respChan <- rscUpdateMeta
				time.Sleep(10 * time.Millisecond)

				config := ResourceConfigRequest{
					ltmConfig:  mockCtlr.resources.getLTMConfigDeepCopy(),
					shareNodes: mockCtlr.shareNodes,
					gtmConfig:  mockCtlr.resources.getGTMConfigCopy(),
				}
				config.reqId = mockCtlr.Controller.enqueueReq(config)
				mockCtlr.Agent.respChan <- rscUpdateMeta

				rscUpdateMeta.failedTenants["test"] = struct{}{}
				mockCtlr.Agent.respChan <- rscUpdateMeta

				time.Sleep(10 * time.Millisecond)
			})
		})

		Describe("Processing Transport Server", func() {
			var ts *cisapiv1.TransportServer
			var fooEndpts *v1.Endpoints
			var fooPorts []v1.ServicePort

			BeforeEach(func() {
				//Add Virtual Server
				fooPorts := []v1.ServicePort{{Port: 80, NodePort: 30001},
					{Port: 8080, NodePort: 38001},
					{Port: 9090, NodePort: 39001}}
				fooIps := []string{"10.1.1.1"}

				fooEndpts = test.NewEndpoints(
					"svc1", "1", "node0", namespace, fooIps, []string{},
					convertSvcPortsToEndpointPorts(fooPorts))

				//Add Virtual Server
				ts = test.NewTransportServer(
					"SampleTS",
					namespace,
					cisapiv1.TransportServerSpec{
						VirtualServerAddress: "10.1.1.1",
						PolicyName:           "policy",
						Pool: cisapiv1.Pool{
							Service:     "svc1",
							ServicePort: 80,
							Monitor: cisapiv1.Monitor{
								Type:     "tcp",
								Timeout:  10,
								Interval: 10,
							},
						},
						BotDefense:         "/Common/bot-defense",
						DOS:                "/Common/dos",
						IRules:             []string{"/Common/SampleIRule"},
						PersistenceProfile: "source-address",
						AllowVLANs:         []string{"/Common/devtraffic"},
						Profiles: cisapiv1.ProfileSpec{
							TCP: cisapiv1.ProfileTCP{
								Client: "/Common/f5-tcp-lan",
								Server: "/Common/f5-tcp-wan",
							},
							ProfileL4: "/Common/security-fastL4",
						},
					},
				)

			})

			It("Transport Server Validation", func() {
				go mockCtlr.Agent.agentWorker()
				go mockCtlr.Agent.retryWorker()
				go mockCtlr.responseHandler(mockCtlr.Agent.respChan)

				mockCtlr.addEndpoints(fooEndpts)
				mockCtlr.processResources()

				svc := test.NewService("svc1", "1", namespace, "NodePort", fooPorts)
				mockCtlr.addService(svc)
				mockCtlr.processResources()

				//check if virtual server exist
				valid := mockCtlr.checkValidTransportServer(ts)
				Expect(valid).To(BeFalse())

				// with invalid type
				ts.Spec.Type = "sctp1"
				mockCtlr.addTransportServer(ts)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid Transport Server")

				mockCtlr.deleteTransportServer(ts)
				mockCtlr.processResources()

				// with missing policy
				ts.Spec.Type = "tcp"
				ts.Spec.VirtualServerAddress = "10.0.0.1"
				mockCtlr.addTransportServer(ts)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid Transport Server")

				mockCtlr.addPolicy(policy)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Transport Server not processed")

				rscUpdateMeta := resourceStatusMeta{
					0,
					make(map[string]struct{}),
				}

				mockCtlr.Agent.respChan <- rscUpdateMeta

				config := ResourceConfigRequest{
					ltmConfig:  mockCtlr.resources.getLTMConfigDeepCopy(),
					shareNodes: mockCtlr.shareNodes,
					gtmConfig:  mockCtlr.resources.getGTMConfigCopy(),
				}
				config.reqId = mockCtlr.Controller.enqueueReq(config)
				config.reqId = mockCtlr.Controller.enqueueReq(config)
				rscUpdateMeta.id = 3
				mockCtlr.Agent.respChan <- rscUpdateMeta

				rscUpdateMeta.failedTenants["test"] = struct{}{}
				config.reqId = mockCtlr.Controller.enqueueReq(config)
				config.reqId = mockCtlr.Controller.enqueueReq(config)
				rscUpdateMeta.id = 3

				mockCtlr.Agent.respChan <- rscUpdateMeta

				time.Sleep(10 * time.Millisecond)

			})

			It("Transport Server with IPAM", func() {
				go mockCtlr.Agent.agentWorker()
				go mockCtlr.Agent.retryWorker()
				mockCtlr.TeemData.ResourceType.IPAMTS = make(map[string]int)
				//Add Service
				mockCtlr.addEndpoints(fooEndpts)
				mockCtlr.processResources()

				svc := test.NewService("svc1", "1", namespace, "NodePort", fooPorts)
				mockCtlr.addService(svc)
				mockCtlr.processResources()

				mockCtlr.addTransportServer(ts)
				mockCtlr.processResources()

				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid Transport Server")

				mockCtlr.addPolicy(policy)
				mockCtlr.processResources()

				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Transport Server not processed")

				//check if virtual server exist
				ts.Spec.VirtualServerAddress = ""
				valid := mockCtlr.checkValidTransportServer(ts)
				Expect(valid).To(BeFalse())

				ts.Spec.VirtualServerAddress = "10.1.1.1"
				mockCtlr.deleteTransportServer(ts)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Transport Server not deleted")

				//mockCtlr.ipamCli = ipammachinery.NewFakeIPAMClient(nil, nil, nil)
				ts.Spec.VirtualServerAddress = ""
				ts.Spec.IPAMLabel = "test"
				mockCtlr.addTransportServer(ts)
				mockCtlr.processResources()

				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid Transport Server")

				var key string

				ipamCR := mockCtlr.getIPAMCR()
				key = "default/SampleTS_ts"

				ipamCR.Spec.HostSpecs = []*ficV1.HostSpec{
					{
						IPAMLabel: "test",
						Key:       key,
					},
				}
				ipamCR, _ = mockCtlr.ipamCli.Update(ipamCR)
				newIpamCR := ipamCR.DeepCopy()

				newIpamCR.Status.IPStatus = []*ficV1.IPSpec{
					{
						IPAMLabel: "test",
						Host:      "",
						IP:        "10.1.1.2",
						Key:       key,
					},
				}
				newIpamCR, _ = mockCtlr.ipamCli.Update(newIpamCR)

				mockCtlr.enqueueUpdatedIPAM(ipamCR, newIpamCR)
				mockCtlr.processResources()

				_, status := mockCtlr.requestIP("test", "", key)

				Expect(status).To(Equal(Allocated), "Failed to fetch Allocated IP")
				mockCtlr.deleteTransportServer(ts)
				mockCtlr.processResources()

				ts.Spec.HostGroup = "hg"
				ts.Spec.VirtualServerAddress = ""
				mockCtlr.addTransportServer(ts)
				mockCtlr.processResources()

				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid Transport Server")

				key = "hg_hg"
				newIpamCR.Spec.HostSpecs = []*ficV1.HostSpec{
					{
						IPAMLabel: "test",
						Key:       key,
					},
				}
				newIpamCR, _ = mockCtlr.ipamCli.Update(newIpamCR)
				ipamCR = newIpamCR.DeepCopy()

				newIpamCR.Status.IPStatus = []*ficV1.IPSpec{
					{
						IPAMLabel: "test",
						//Host:      host,
						IP:  "10.10.10.1",
						Key: key,
					},
				}
				newIpamCR, _ = mockCtlr.ipamCli.Update(newIpamCR)

				mockCtlr.enqueueUpdatedIPAM(ipamCR, newIpamCR)
				mockCtlr.processResources()

				_, status = mockCtlr.requestIP("test", "", key)
				Expect(status).To(Equal(Allocated), "Failed to fetch Allocated IP")

				mockCtlr.ipamCli = nil
				mockCtlr.addTransportServer(ts)
				mockCtlr.processResources()
			})
		})

		Describe("Processing EDNS", func() {
			var fooEndpts *v1.Endpoints
			var fooPorts []v1.ServicePort
			var newEDNS *cisapiv1.ExternalDNS

			BeforeEach(func() {
				//Add Virtual Server
				fooPorts := []v1.ServicePort{{Port: 80, NodePort: 30001},
					{Port: 8080, NodePort: 38001},
					{Port: 9090, NodePort: 39001}}
				fooIps := []string{"10.1.1.1"}

				fooEndpts = test.NewEndpoints(
					"svc1", "1", "node0", namespace, fooIps, []string{},
					convertSvcPortsToEndpointPorts(fooPorts))

				//Add EDNS
				newEDNS = test.NewExternalDNS(
					"SampleEDNS",
					"default",
					cisapiv1.ExternalDNSSpec{
						DomainName: "test.com",
						Pools: []cisapiv1.DNSPool{
							{
								DataServerName: "DataServer",
								Monitor: cisapiv1.Monitor{
									Type:     "http",
									Send:     "GET /health",
									Interval: 10,
									Timeout:  10,
								},
							},
						},
					})
			})

			It("EDNS", func() {
				//Add Service
				//go mockCtlr.Agent.agentWorker()
				//go mockCtlr.Agent.retryWorker()
				mockCtlr.addEndpoints(fooEndpts)
				mockCtlr.processResources()

				svc := test.NewService("svc1", "1", namespace, "NodePort", fooPorts)
				mockCtlr.addService(svc)
				mockCtlr.processResources()

				mockCtlr.addEDNS(newEDNS)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.gtmConfig)).To(Equal(1), "EDNS not processed")

				mockCtlr.deleteEDNS(newEDNS)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.gtmConfig["test"].WideIPs)).To(Equal(0), "EDNS  not deleted")

			})
		})

		Describe("Processing Ingress Link", func() {
			It("Ingress Link", func() {
				go mockCtlr.Agent.agentWorker()
				go mockCtlr.Agent.retryWorker()
				fooPorts := []v1.ServicePort{
					{
						Port: 8080,
						Name: "port0",
					},
				}
				foo := test.NewService("foo", "1", namespace, v1.ServiceTypeClusterIP, fooPorts)
				label1 := make(map[string]string)
				label1["app"] = "ingresslink"
				foo.ObjectMeta.Labels = label1
				var (
					selector = &metav1.LabelSelector{
						MatchLabels: label1,
					}
				)

				mockCtlr.kubeClient.CoreV1().Services("default").Create(context.TODO(), foo, metav1.CreateOptions{})
				mockCtlr.addService(foo)
				mockCtlr.processResources()

				var iRules []string
				IngressLink1 := test.NewIngressLink("ingresslink1", namespace, "1",
					cisapiv1.IngressLinkSpec{
						Selector: selector,
						IRules:   iRules,
					})
				mockCtlr.TeemData = &teem.TeemsData{
					ResourceType: teem.ResourceTypes{
						IngressLink: make(map[string]int),
					},
				}
				IngressLink1.Spec.IPAMLabel = "test"

				valid := mockCtlr.checkValidIngressLink(IngressLink1)
				Expect(valid).To(BeFalse())

				mockCtlr.addIngressLink(IngressLink1)
				mockCtlr.processResources()

				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid IngressLink")

				var key, host string
				var status int

				ipamCR := mockCtlr.getIPAMCR()
				key = "default/ingresslink1_il"

				ipamCR.Spec.HostSpecs = []*ficV1.HostSpec{
					{
						IPAMLabel: "test",
						Key:       key,
					},
				}
				ipamCR, _ = mockCtlr.ipamCli.Update(ipamCR)
				newIpamCR := ipamCR.DeepCopy()

				newIpamCR.Status.IPStatus = []*ficV1.IPSpec{
					{
						IPAMLabel: "test",
						IP:        "10.10.10.1",
						Key:       key,
					},
				}
				newIpamCR, _ = mockCtlr.ipamCli.Update(newIpamCR)

				mockCtlr.enqueueUpdatedIPAM(ipamCR, newIpamCR)
				mockCtlr.processResources()

				_, status = mockCtlr.requestIP("test", host, key)
				Expect(status).To(Equal(Allocated), "Failed to fetch Allocated IP")
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "IngressLink not processed")
				mockCtlr.deleteIngressLink(IngressLink1)
				mockCtlr.processResources()

				IngressLink1.Spec.VirtualServerAddress = "10.10.10.1"
				mockCtlr.addIngressLink(IngressLink1)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "IngressLink not processed")
				ilList := mockCtlr.getAllIngLinkFromMonitoredNamespaces()
				Expect(len(ilList)).To(Equal(1))
				ilList = mockCtlr.getAllIngressLinks("")
				Expect(len(ilList)).To(Equal(0))
				mockCtlr.crInformers[""] = mockCtlr.newNamespacedCustomResourceInformer("")
				mockCtlr.crInformers[""].ilInformer.GetStore().Add(IngressLink1)
				ilList = mockCtlr.getAllIngressLinks("")
				Expect(len(ilList)).To(Equal(1))
				ilList = mockCtlr.getAllIngLinkFromMonitoredNamespaces()
				Expect(len(ilList)).To(Equal(1))
				delete(mockCtlr.crInformers, "")
				IngressLink1.Spec.IPAMLabel = ""
				IngressLink1.Spec.VirtualServerAddress = ""
				valid = mockCtlr.checkValidIngressLink(IngressLink1)
				Expect(valid).To(BeFalse(), "Invalid IngressLink")

				mockCtlr.ipamCli = nil
				IngressLink1.Spec.VirtualServerAddress = ""
				valid = mockCtlr.checkValidIngressLink(IngressLink1)
				Expect(valid).To(BeFalse(), "Invalid IngressLink")

			})
		})
	})

	Describe("Processing Native Resources", func() {
		var mockPM *mockPostManager
		BeforeEach(func() {
			mockCtlr.mode = OpenShiftMode
			mockCtlr.namespaces = make(map[string]bool)
			mockCtlr.namespaces["default"] = true
			mockCtlr.kubeCRClient = crdfake.NewSimpleClientset()
			mockCtlr.routeClientV1 = fakeRouteClient.NewSimpleClientset().RouteV1()
			mockCtlr.kubeClient = k8sfake.NewSimpleClientset()
			mockCtlr.nrInformers = make(map[string]*NRInformer)
			mockCtlr.comInformers = make(map[string]*CommonInformer)
			mockCtlr.nativeResourceSelector, _ = createLabelSelector(DefaultNativeResourceLabel)
			mockCtlr.PoolMemberType = NodePortLocal
			mockCtlr.nrInformers["default"] = mockCtlr.newNamespacedNativeResourceInformer("default")
			mockCtlr.nrInformers["test"] = mockCtlr.newNamespacedNativeResourceInformer("test")
			mockCtlr.comInformers["test"] = mockCtlr.newNamespacedCommonResourceInformer("test")
			mockCtlr.comInformers["default"] = mockCtlr.newNamespacedCommonResourceInformer("default")
			mockCtlr.nrInformers["system"] = mockCtlr.newNamespacedNativeResourceInformer("system")
			var processedHostPath ProcessedHostPath
			processedHostPath.processedHostPathMap = make(map[string]metav1.Time)
			mockCtlr.processedHostPath = &processedHostPath
			mockCtlr.resourceQueue = workqueue.NewNamedRateLimitingQueue(
				workqueue.DefaultControllerRateLimiter(), "custom-resource-controller")
			mockCtlr.resources = NewResourceStore()
			mockCtlr.comInformers["default"] = mockCtlr.newNamespacedCommonResourceInformer("default")

			mockCtlr.TeemData = &teem.TeemsData{
				ResourceType: teem.ResourceTypes{
					RouteGroups:     make(map[string]int),
					NativeRoutes:    make(map[string]int),
					ExternalDNS:     make(map[string]int),
					IngressLink:     make(map[string]int),
					VirtualServer:   make(map[string]int),
					TransportServer: make(map[string]int),
				},
			}

			mockCtlr.requestQueue = &requestQueue{sync.Mutex{}, list.New()}
			err := mockCtlr.addNamespacedInformers(namespace, false)
			Expect(err).To(BeNil(), "Informers Creation Failed")

			mockCtlr.Agent = &Agent{
				postChan:            make(chan ResourceConfigRequest, 1),
				cachedTenantDeclMap: make(map[string]as3Tenant),
				respChan:            make(chan resourceStatusMeta, 1),
				retryTenantDeclMap:  make(map[string]*tenantParams),
			}

			mockPM = newMockPostManger()
			mockPM.BIGIPURL = "bigip.com"
			mockPM.BIGIPUsername = "user"
			mockPM.BIGIPPassword = "pswd"
			mockPM.tenantResponseMap = make(map[string]tenantResponse)
			mockPM.LogResponse = true
			//					mockPM.AS3PostDelay =
			mockPM.setupBIGIPRESTClient()
			tnt := "test"
			mockPM.setResponses([]responceCtx{{
				tenant: tnt,
				status: http.StatusOK,
				body:   "",
			}}, http.MethodPost)
			mockPM.firstPost = false
			mockCtlr.Agent.PostManager = mockPM.PostManager

			mockCtlr.ipamCli = ipammachinery.NewFakeIPAMClient(nil, nil, nil)
			_ = mockCtlr.createIPAMResource()

		})
		AfterEach(func() {
			mockCtlr.shutdown()
		})

		Describe("Process ConfigMap", func() {
			var cm *v1.ConfigMap
			var localCM *v1.ConfigMap
			data := make(map[string]string)
			BeforeEach(func() {
				cmName := "samplecfgmap"
				mockCtlr.routeSpecCMKey = namespace + "/" + cmName
				routeGroup := "default"
				mockCtlr.resources.extdSpecMap[routeGroup] = &extendedParsedSpec{
					override: true,
					global: &ExtendedRouteGroupSpec{
						VServerName:   "nextgenroutes",
						VServerAddr:   "10.10.10.10",
						AllowOverride: "False",
					},
					namespaces: []string{routeGroup},
					partition:  "test",
				}

				cm = test.NewConfigMap(
					cmName,
					"v1",
					"default",
					data)

				data["extendedSpec"] = `
baseRouteSpec: 
    tlsCipher:
      tlsVersion : 1.2
      ciphers: DEFAULT
      cipherGroup: /Common/f5-default
    defaultTLS:
       clientSSL: /Common/clientssl
       serverSSL: /Common/serverssl
       reference: bigip
    defaultRouteGroup: 
       bigIpPartition: test
       vserverAddr: 10.1.1.1
       allowOverride: false

extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: true
    - namespace: test
      vserverAddr: 10.8.3.12
      vserverName: nextgenroutes
      allowOverride: true
    - namespace: new
      vserverAddr: 10.8.3.13
      vserverName: nextgenroutes
      allowOverride: true
`
				localData := make(map[string]string)
				localCM = test.NewConfigMap(
					"localESCM",
					"v1",
					"default",
					localData)
				localData["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.110
      vserverName: nextgenroutes
      policyCR : default/policy
`
			})

			It("Process Global ConfigMap", func() {
				mockCtlr.initState = true
				mockCtlr.processConfigMap(cm, false)
				mockCtlr.processConfigMap(localCM, false)
				mockCtlr.initState = false
				mockCtlr.processConfigMap(cm, false)
				mockCtlr.processConfigMap(localCM, false)

				data["extendedSpec"] = `
baseRouteSpec: 
    tlsCipher:
      tlsVersion : 1.2
      ciphers: DEFAULT
      cipherGroup: /Common/f5-default
    defaultTLS:
       clientSSL: /Common/clientssl
       serverSSL: /Common/serverssl
       reference: bigip
    defaultRouteGroup: 
       bigIpPartition: test
       vserverAddr: 10.1.1.1
       allowOverride: false
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.17
      vserverName: nextgenroutes
      allowOverride: true 
 	- namespace: app
      vserverAddr: 10.8.3.15
      allowOverride: true
`
				mockCtlr.processConfigMap(cm, false)
				mockCtlr.processConfigMap(localCM, false)
			})
			It("Process Local ConfigMap", func() {

			})
		})
		Describe("Process Route", func() {
			AfterEach(func() {
				mockCtlr.shutdown()
			})

			var fooEndpts *v1.Endpoints
			var fooPorts []v1.ServicePort
			var spec1 routeapi.RouteSpec
			var routeGroup = "default"
			var svc *v1.Service
			var policy *cisapiv1.Policy
			var cm *v1.ConfigMap
			var localCM *v1.ConfigMap
			var annotation1 map[string]string

			BeforeEach(func() {
				fooPorts = []v1.ServicePort{{Port: 80, NodePort: 30001},
					{Port: 8080, NodePort: 38001},
					{Port: 9090, NodePort: 39001}}
				svc = test.NewService("foo", "1", routeGroup, "ClusterIP", fooPorts)

				fooIps := []string{"10.1.1.1"}
				fooEndpts = test.NewEndpoints(
					"foo", "1", "node0", routeGroup, fooIps, []string{},
					convertSvcPortsToEndpointPorts(fooPorts))

				mockCtlr.resources = NewResourceStore()
				mockCtlr.resources.extdSpecMap[routeGroup] = &extendedParsedSpec{
					override: true,
					global: &ExtendedRouteGroupSpec{
						VServerName:   "nextgenroutes",
						VServerAddr:   "10.10.10.10",
						AllowOverride: "False",
					},
					namespaces: []string{routeGroup},
					partition:  "test",
				}

				//Add Service

				mockCtlr.resources.extdSpecMap[routeGroup] = &extendedParsedSpec{
					override: true,
					global: &ExtendedRouteGroupSpec{
						VServerName:   "nextgenroutes",
						VServerAddr:   "10.10.10.10",
						AllowOverride: "False",
					},
					namespaces: []string{routeGroup},
					partition:  "test",
				}

				spec1 = routeapi.RouteSpec{
					Host: "pytest-foo-1.com",
					Path: "/test",
					Port: &routeapi.RoutePort{
						TargetPort: intstr.IntOrString{
							IntVal: 80,
						},
					},
					To: routeapi.RouteTargetReference{
						Kind: "Service",
						Name: "foo",
					},
					TLS: &routeapi.TLSConfig{Termination: "reencrypt",
						Certificate:              "-----BEGIN CERTIFICATE-----\nMIIC+DCCAeCgAwIBAgIQIBIcC6PuJQEHwwI0Hv5QmTANBgkqhkiG9w0BAQsFADAS\nMRAwDgYDVQQKEwdBY21lIENvMB4XDTIyMTIyMjA5MjE0OFoXDTIzMTIyMjA5MjE0\nOFowEjEQMA4GA1UEChMHQWNtZSBDbzCCASIwDQYJKoZIhvcNAQEBBQADggEPADCC\nAQoCggEBAN0NWXsUvGYBV9uo2Iuz3gnovyk3W7p8AA4I8eRUFaWV1EYaxFpsGmdN\nrQgdVJ6w+POSykbDuZynYJyBjC11dJmfTaXffLaUSrJfu+a0QaeWIpt+XxzO4SKQ\nunUSh5Z9w4P45G8VKF7E67wFVN0ni10FLAfBUjYVsQpPagpkH8OdnYCsymCzVSWi\nYETZZ+Hbaih9flRgBQOsoUyNBSkCdJ2wEkZ/0p9+tYwZp1Xvp/Neu3TTsezpu7lE\nbTp0RLQNqfLHWiMV9BSAQRbXAvtvky3J42iy+ec24JyQPtiD85u8Pp/+ssV0ZL9l\nc5KoDEuAvf4NPFWu270gYyQljKcTbB8CAwEAAaNKMEgwDgYDVR0PAQH/BAQDAgWg\nMBMGA1UdJQQMMAoGCCsGAQUFBwMBMAwGA1UdEwEB/wQCMAAwEwYDVR0RBAwwCoII\ndGVzdC5jb20wDQYJKoZIhvcNAQELBQADggEBAI9VUdpVmfx+WUEejREa+plEjCIV\ns+d7v66ddyU4B+Zer1y4RgoWaVq5pywPPjBNJuz6NfwSvBCmuMUd1LUoF5tQFkqb\nVa85Aq6ODbwIMoQ53kTG9vLbT78qESrbukaW9v+axdD9/DIXZJtdwvLvHAVpelRi\n7z48Lxk1GTe7dM3ixKQrU4hz656kH3kXSnD79metOkJA6BAXsqL2XonIhNkCkQVV\n38IHDNkzk228d97ebLu+EhLlkjFgFQEnXusK1amrGJrRDli72pY01yxzGI1caKG5\nN6I8MEIqYI/POwbYWENqONF22pzw/OIs4T1a3jjUqEFugnELcTtx/xRLmOI=\n-----END CERTIFICATE-----\n",
						Key:                      "-----BEGIN PRIVATE KEY-----\nMIIEvAIBADANBgkqhkiG9w0BAQEFAASCBKYwggSiAgEAAoIBAQDdDVl7FLxmAVfb\nqNiLs94J6L8pN1u6fAAOCPHkVBWlldRGGsRabBpnTa0IHVSesPjzkspGw7mcp2Cc\ngYwtdXSZn02l33y2lEqyX7vmtEGnliKbfl8czuEikLp1EoeWfcOD+ORvFShexOu8\nBVTdJ4tdBSwHwVI2FbEKT2oKZB/DnZ2ArMpgs1UlomBE2Wfh22oofX5UYAUDrKFM\njQUpAnSdsBJGf9KffrWMGadV76fzXrt007Hs6bu5RG06dES0Danyx1ojFfQUgEEW\n1wL7b5MtyeNosvnnNuCckD7Yg/ObvD6f/rLFdGS/ZXOSqAxLgL3+DTxVrtu9IGMk\nJYynE2wfAgMBAAECggEAf8l91vcvylAweB1twaUjUNsp1yvXbUDNz09Adtxc/zJU\nWoqSxCsGQH3Y7331Mx/fav+Ky8nN/U+NPCxv2r+xvjUncCJ4OBwV6nQJbd76rWTP\ncNBnL4IxCAheodsqYsclRZ+WftjeU5rHJBR48Lgxin6462rImdeEVw99n7At5Kig\nGZmGNXnk6jgvoNU1YJZxSMWQQwKtrfJxXry5a90SfjiviGseuBPsgbrMxEPaeqlQ\nGAMi4nIVRmijL56vbbuuudZm+6dpOnbGzzF6J4M5Nrfr/qJF7ClwXjcMeb6lESIo\n5pmGl3QwSGQYeflFexP3ydvQdUwN5rLbtCexPC2CsQKBgQDxLPn8pIU7WuFiTuOp\n1o7/25v7ijPydIRBjjVeA7E7+mbq9FllkT4CW+HtP7zCCjdScuXhKjuPRrST4fsZ\nZex2nUYfc586s/W95b4QMKtXcJd1MMMWOK2/ZGN/6L5zLPupDrhyWHw91biFZG8h\nSFgn7G2zS/+09gJTglpdj3gClQKBgQDqo7f+kZiXGFvP4kcOWNgnOJOpdqzG/zeD\nuVP2Y6Q8mi7GhkiYhdlrl6Ibh9X0qjFMKMKy827jbUPSGaj5tIT8iXyFT4KVaqZQ\n7r2cMyCqbznKfWlyMyspaVEDa910+VwC2hYQvahTQzfdQqFp6JfiLqCdQtiNDGLf\nbvUOHk4a4wKBgHDLo0NowrMm5wBuewXExm6djE9RrMf5fJ2YYBdPTMYLb7T1gRYC\nnujFhl3KkIKD+qnB+QedE+wHmo8Lgr+3LqevGMu+7LqszgL5fzHdQVWM4Bk8LBGp\ngoFf9zUsal49rJm9u8Am6DyXR0yD04HSbwCFEC1qHvbIk//wmEjnv64dAoGANBbW\nYPBHlLt2nmbYaWn1ync36LYM0zyTQW3iIt+p9T4xRidHdHy6cLU/6qa0K9Wgjgy6\ndGmwY1K9bKX/qjeWEk4fU6T8E1mSxILLmyKKjOuWQ8qlnxGW8mGL95t5lV9KOuPZ\nZCwGcz2H6FnDZbSaCz9YrrDJTD7EsF98jX7SzgsCgYBQv5yi7aGxH6OcrAJPQH4v\n1fZo7mFbqp0WoUMpwuWKNOHZuZoF0EU/bllMZT7AipxVhso+hUC+rDEO7H36TAyc\nTUJbdxtlIC1JmJTmeBOWh3i3Htu8A97DLUNTqNikNyKyGWjy7eC0ncG3+CGG91wA\nky9KxzxszaIez6kIUCY7xQ==\n-----END PRIVATE KEY-----\n",
						DestinationCACertificate: "     -----BEGIN CERTIFICATE-----\n      MIIDVzCCAj+gAwIBAgIJANFsRSwLRI09MA0GCSqGSIb3DQEBCwUAMEIxCzAJBgNV\n      BAYTAlVTMQswCQYDVQQIDAJDTzEMMAoGA1UEBwwDQkRPMQswCQYDVQQLDAJjYTEL\n      MAkGA1UECgwCRjUwHhcNMjIxMTI4MDc0NTIwWhcNMjMxMTI4MDc0NTIwWjBCMQsw\n      CQYDVQQGEwJVUzELMAkGA1UECAwCQ08xDDAKBgNVBAcMA0JETzELMAkGA1UECwwC\n      Y2ExCzAJBgNVBAoMAkY1MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA\n      sCDsFH9xMWAPD8VNOnLPJi49jJ5hRhQcx5oYP9FVvHDvRv0PQKXOfA1BOZeGSOjK\n      3p0k4SVXFBg4EHMBVhW3NRFIR0JppoyrF8Jj9Ts83D5N6eLW1ShYJnXrqrEKhjI1\n      c0e+Eta+LsVwktRLQCABsb2Ca/J/PUpD450i9ss11wGzX3lHg2XKr8vUZGkJRJmB\n      iqHV1Ahm9y9SDceyWQ9AESq+sKkz/swoEAwi1Vb2Bmri8aj7a0hlCVy59dngPayP\n      jhqoJDxMOThGaNn4EcOKuPtqfJ6CwOyOEFdc6DGnEuTdXpMbj+L8V1R1mgyZX/uo\n      OIhJkEG0aPz3ZB7Ks94ykQIDAQABo1AwTjAdBgNVHQ4EFgQUlOlkzFc3BF8jhoDv\n      rhMthDD3TDQwHwYDVR0jBBgwFoAUlOlkzFc3BF8jhoDvrhMthDD3TDQwDAYDVR0T\n      BAUwAwEB/zANBgkqhkiG9w0BAQsFAAOCAQEAmyvLI5T8dqJVzWXwwVTRwX8ca2Li\n      Y53oiChuBTtT2TUyRoeWFSqN5QwpxvexFxLpCUWdO1v+GjeLIKyYa2yq86Gr4oi3\n      NNBy+8BA2q092AKWDSMqGhw0COJMapoWKWekwbXw+yOBwfpM36+OrHbRRf/00USv\n      VwNB+TJA1MvdiXRs9WXCIqtOjEcQwFTzQSnhejyXMUWodEsooTmnLENfskdCAoMp\n      oqUPGMn1HxiPewweP6Hh+up4g6accrZ59pBaeQ+t4xrevUSh0CUqx5xobOhB2Z8S\n      Dn7eCxGJDOKqJ2TZWFQWEw6Gk6gRpbNHL96HPztQUnp6dgyLjX2USHbMZg==\n      -----END CERTIFICATE-----",
						CACertificate:            "     -----BEGIN CERTIFICATE-----\n      MIIDVzCCAj+gAwIBAgIJANFsRSwLRI09MA0GCSqGSIb3DQEBCwUAMEIxCzAJBgNV\n      BAYTAlVTMQswCQYDVQQIDAJDTzEMMAoGA1UEBwwDQkRPMQswCQYDVQQLDAJjYTEL\n      MAkGA1UECgwCRjUwHhcNMjIxMTI4MDc0NTIwWhcNMjMxMTI4MDc0NTIwWjBCMQsw\n      CQYDVQQGEwJVUzELMAkGA1UECAwCQ08xDDAKBgNVBAcMA0JETzELMAkGA1UECwwC\n      Y2ExCzAJBgNVBAoMAkY1MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA\n      sCDsFH9xMWAPD8VNOnLPJi49jJ5hRhQcx5oYP9FVvHDvRv0PQKXOfA1BOZeGSOjK\n      3p0k4SVXFBg4EHMBVhW3NRFIR0JppoyrF8Jj9Ts83D5N6eLW1ShYJnXrqrEKhjI1\n      c0e+Eta+LsVwktRLQCABsb2Ca/J/PUpD450i9ss11wGzX3lHg2XKr8vUZGkJRJmB\n      iqHV1Ahm9y9SDceyWQ9AESq+sKkz/swoEAwi1Vb2Bmri8aj7a0hlCVy59dngPayP\n      jhqoJDxMOThGaNn4EcOKuPtqfJ6CwOyOEFdc6DGnEuTdXpMbj+L8V1R1mgyZX/uo\n      OIhJkEG0aPz3ZB7Ks94ykQIDAQABo1AwTjAdBgNVHQ4EFgQUlOlkzFc3BF8jhoDv\n      rhMthDD3TDQwHwYDVR0jBBgwFoAUlOlkzFc3BF8jhoDvrhMthDD3TDQwDAYDVR0T\n      BAUwAwEB/zANBgkqhkiG9w0BAQsFAAOCAQEAmyvLI5T8dqJVzWXwwVTRwX8ca2Li\n      Y53oiChuBTtT2TUyRoeWFSqN5QwpxvexFxLpCUWdO1v+GjeLIKyYa2yq86Gr4oi3\n      NNBy+8BA2q092AKWDSMqGhw0COJMapoWKWekwbXw+yOBwfpM36+OrHbRRf/00USv\n      VwNB+TJA1MvdiXRs9WXCIqtOjEcQwFTzQSnhejyXMUWodEsooTmnLENfskdCAoMp\n      oqUPGMn1HxiPewweP6Hh+up4g6accrZ59pBaeQ+t4xrevUSh0CUqx5xobOhB2Z8S\n      Dn7eCxGJDOKqJ2TZWFQWEw6Gk6gRpbNHL96HPztQUnp6dgyLjX2USHbMZg==\n      -----END CERTIFICATE-----",
					},
				}

				//Policy

				policy = &cisapiv1.Policy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "policy",
						Namespace: "default",
					},
					Spec: cisapiv1.PolicySpec{
						SNAT: "auto",
						L7Policies: cisapiv1.L7PolicySpec{
							WAF: "/Common/WAF_Policy",
						},
						L3Policies: cisapiv1.L3PolicySpec{
							FirewallPolicy: "/Common/AFM_Policy",
							DOS:            "/Common/dos",
							BotDefense:     "/Common/bot-defense",
							AllowSourceRange: []string{
								"1.1.1.0/24",
								"2.2.2.0/24",
							},
							AllowVlans: []string{
								" /Common/external",
							},
						},
						Profiles: cisapiv1.ProfileSpec{
							TCP: cisapiv1.ProfileTCP{
								Client: "/Common/f5-tcp-lan",
								Server: "/Common/f5-tcp-wan",
							},
							HTTP:  "/Common/http",
							HTTP2: "/Common/http2",
							LogProfiles: []string{
								"/Common/Log all requests", "/Common/local-dos"},
							ProfileL4:        " /Common/security-fastL4",
							ProfileMultiplex: "/Common/oneconnect",
							UDP:              "/Common/udp",
						},
					},
				}

				// ConfigMap
				cmName := "escm"
				cmNamespace := "system"
				mockCtlr.routeSpecCMKey = cmNamespace + "/" + cmName
				mockCtlr.resources = NewResourceStore()
				data := make(map[string]string)
				cm = test.NewConfigMap(
					cmName,
					"v1",
					cmNamespace,
					data)

				data["extendedSpec"] = `
baseRouteSpec: 
    tlsCipher:
      tlsVersion : 1.2
      ciphers: DEFAULT
      cipherGroup: /Common/f5-default
    defaultTLS:
       clientSSL: /Common/clientssl
       serverSSL: /Common/serverssl
       reference: bigip
    defaultRouteGroup: 
       bigIpPartition: test
       vserverAddr: vs
       allowOverride: false

extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: true
      policyCR : default/policy
`
				localData := make(map[string]string)
				localCM = test.NewConfigMap(
					"localESCM",
					"v1",
					"default",
					localData)
				localData["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.110
      vserverName: nextgenroutes
      policyCR : default/policy
`

				//Annotations
				annotation1 = make(map[string]string)
				annotation1[resource.F5ClientSslProfileAnnotation] = "/Common/clientssl"
				annotation1[resource.F5ServerSslProfileAnnotation] = "/Common/serverssl"
				annotation1[resource.F5VsBalanceAnnotation] = "least-connections-node"
				annotation1[resource.F5VsURLRewriteAnnotation] = "/foo"
				annotation1[resource.HealthMonitorAnnotation] = "[{\"path\": \"pytest-foo-1.com/\",\"send\": \"HTTP GET pytest-foo-1.com/\", \"recv\": \"\",\"interval\": 2,\"timeout\": 5,  \"type\": \"https\"}]"

			})

			It("Process Re-encrypt Route", func() {

				mockCtlr.resources.invertedNamespaceLabelMap[namespace] = routeGroup

				mockCtlr.addConfigMap(cm)
				mockCtlr.processResources()
				mockCtlr.Agent.ccclGTMAgent = true
				writer := &test.MockWriter{
					FailStyle: test.Success,
					Sections:  make(map[string]interface{}),
				}
				mockCtlr.Agent.ConfigWriter = writer
				go mockCtlr.Agent.agentWorker()
				go mockCtlr.Agent.retryWorker()

				routeGroup := "default"
				mockCtlr.addPolicy(policy)
				mockCtlr.processResources()

				labels := make(map[string]string)
				labels["app"] = "UpdatePoolHealthMonitors"
				svc.Spec.Selector = labels
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod",
						Labels:    labels,
						Namespace: namespace,
					},
				}
				Handler := v1.Handler{
					HTTPGet: &v1.HTTPGetAction{
						Path: "/",
						Port: intstr.IntOrString{
							IntVal: 80,
						},
						Scheme: HTTP,
					},
				}
				cnt := v1.Container{
					LivenessProbe: &v1.Probe{
						FailureThreshold: 3,
						TimeoutSeconds:   10,
						PeriodSeconds:    10,
						SuccessThreshold: 1,
						Handler:          Handler,
					},
					Ports: []v1.ContainerPort{
						v1.ContainerPort{
							ContainerPort: 80,
							Protocol:      v1.ProtocolTCP,
						},
					},
				}
				pod.Spec.Containers = append(pod.Spec.Containers, cnt)

				mockCtlr.addService(svc)
				mockCtlr.processResources()

				mockCtlr.addPod(pod)
				mockCtlr.processResources()

				mockCtlr.addEndpoints(fooEndpts)
				mockCtlr.processResources()

				mockCtlr.deleteEndpoints(fooEndpts)
				mockCtlr.processResources()

				mockCtlr.addEndpoints(fooEndpts)
				mockCtlr.processResources()

				mockCtlr.deleteService(svc)
				mockCtlr.processResources()

				mockCtlr.addService(svc)
				mockCtlr.processResources()

				route1 := test.NewRoute("route1", "1", routeGroup, spec1, annotation1)
				route1.Spec.TLS.Termination = TLSReencrypt
				//route1.Spec.TLS.Certificate = ""
				//route1.Spec.TLS.Key = ""
				route1.Spec.Host = "test.com"
				delete(route1.Annotations, resource.F5ClientSslProfileAnnotation)

				checkCertificateHost(route1.Spec.Host, []byte(route1.Spec.TLS.Certificate), []byte(route1.Spec.TLS.Key))

				mockCtlr.addRoute(route1)
				mockCtlr.resources.invertedNamespaceLabelMap[routeGroup] = routeGroup
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Route not processed")

				route1.Spec.TLS.Certificate = ""
				route1.Spec.TLS.Key = ""
				mockCtlr.deleteRoute(route1)
				mockCtlr.processResources()

				mockCtlr.addConfigMap(localCM)
				mockCtlr.processResources()

				route1.Annotations[resource.F5ClientSslProfileAnnotation] = "common/client-ssl"
				mockCtlr.addRoute(route1)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Route not processed")

				// Route count should be 0
				Expect(mockCtlr.GetServiceRouteWithoutHealthAnnotation(svc)).To(BeNil())

				pod.Name = "pod1"
				mockCtlr.addPod(pod)
				mockCtlr.processResources()

				mockCtlr.deleteRoute(route1)
				mockCtlr.processResources()

				// Remove health Annotation - This won't work because current we are querying the pods from the kube client instead of informers
				delete(route1.Annotations, resource.HealthMonitorAnnotation)
				mockCtlr.kubeClient.CoreV1().Services(svc.ObjectMeta.Namespace).Create(context.TODO(), svc, metav1.CreateOptions{})
				mockCtlr.kubeClient.CoreV1().Pods(svc.ObjectMeta.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
				mockCtlr.addRoute(route1)
				mockCtlr.resources.invertedNamespaceLabelMap[routeGroup] = routeGroup
				mockCtlr.processResources()

				mockCtlr.deleteEndpoints(fooEndpts)
				mockCtlr.processResources()

				pod.Spec.Containers[0].LivenessProbe.TimeoutSeconds = 1
				mockCtlr.kubeClient.CoreV1().Pods(svc.ObjectMeta.Namespace).Update(context.TODO(), pod, metav1.UpdateOptions{})
				mockCtlr.addEndpoints(fooEndpts)
				mockCtlr.processResources()

				mockCtlr.deleteEndpoints(fooEndpts)
				mockCtlr.processResources()

				mockCtlr.addEndpoints(fooEndpts)
				mockCtlr.processResources()

				//length should be 1
				mockCtlr.getWatchingNamespaces()

				labels["app"] = "test"
				ns := test.NewNamespace(
					"default",
					"1",
					labels,
				)
				mockCtlr.enqueueDeletedNamespace(ns)
				mockCtlr.processResources()

				_, ok := mockCtlr.nsInformers[namespace]
				Expect(ok).To(Equal(false), "Namespace not deleted")

				mockCtlr.Agent.retryFailedTenant()
				//time.Sleep(1 * time.Microsecond)
			})
			It("Process Edge Route", func() {
				go mockCtlr.Agent.agentWorker()
				go mockCtlr.Agent.retryWorker()

				mockCtlr.resources.invertedNamespaceLabelMap[namespace] = routeGroup
				mockCtlr.addConfigMap(cm)
				mockCtlr.processResources()
				routeGroup := "default"

				mockCtlr.addPolicy(policy)
				mockCtlr.processResources()

				mockCtlr.addService(svc)
				mockCtlr.processResources()

				mockCtlr.addEndpoints(fooEndpts)
				mockCtlr.processResources()

				delete(annotation1, resource.F5ClientSslProfileAnnotation)
				delete(annotation1, resource.F5ServerSslProfileAnnotation)
				spec1.Host = "test.com"
				route1 := test.NewRoute("route1", "1", routeGroup, spec1, annotation1)
				route1.Spec.TLS.Termination = TLSEdge
				mockCtlr.addRoute(route1)
				mockCtlr.resources.invertedNamespaceLabelMap[routeGroup] = routeGroup
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Route not processed")

				mockCtlr.deleteRoute(route1)
				mockCtlr.processResources()

				route1.Spec.TLS.Key = ""
				route1.Spec.TLS.Certificate = ""
				mockCtlr.addRoute(route1)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Route not processed")

				mockCtlr.deleteRoute(route1)
				mockCtlr.processResources()

			})
			It("Process Pass-through Route", func() {
				go mockCtlr.responseHandler(mockCtlr.Agent.respChan)
				go mockCtlr.Agent.agentWorker()
				go mockCtlr.Agent.retryWorker()
				mockCtlr.initState = true
				mockCtlr.resources.invertedNamespaceLabelMap[namespace] = routeGroup
				mockCtlr.addConfigMap(cm)
				mockCtlr.processResources()
				mockCtlr.resourceQueue.Get()
				routeGroup := "default"

				mockCtlr.initState = false
				//mockCtlr.DeleteConfigMap(cm)
				//mockCtlr.processResources()

				mockCtlr.resources.invertedNamespaceLabelMap[namespace] = routeGroup
				mockCtlr.mode = CustomResourceMode
				mockCtlr.addConfigMap(cm)
				mockCtlr.processResources()

				mockCtlr.mode = OpenShiftMode
				mockCtlr.addConfigMap(cm)
				mockCtlr.processResources()

				mockCtlr.addService(svc)
				mockCtlr.processResources()

				mockCtlr.addEndpoints(fooEndpts)
				mockCtlr.processResources()

				// Invalid Service
				svcObj := mockCtlr.GetService(svc.Namespace, "sv3")
				Expect(svcObj).To(BeNil())
				//Val
				svcObj = mockCtlr.GetService(svc.Namespace, svc.Name)
				Expect(svcObj).NotTo(BeNil())

				route1 := test.NewRoute("route1", "1", routeGroup, spec1, annotation1)
				route1.Spec.TLS.Termination = TLSPassthrough

				mockCtlr.mode = CustomResourceMode
				mockCtlr.addRoute(route1)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid Controller Mode")

				mockCtlr.mode = OpenShiftMode
				mockCtlr.addRoute(route1)
				mockCtlr.resources.invertedNamespaceLabelMap[routeGroup] = routeGroup
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(0), "Invalid ltm config")

				mockCtlr.addPolicy(policy)
				mockCtlr.processResources()
				Expect(len(mockCtlr.resources.ltmConfig)).To(Equal(1), "Route not processed")
				//Expect(len(mockCtlr.getOrderedRoutes(""))).To(Equal(1), "Invalid no of Routes")
				rscUpdateMeta := resourceStatusMeta{
					0,
					make(map[string]struct{}),
				}

				mockCtlr.routeClientV1.Routes("default").Create(context.TODO(), route1, metav1.CreateOptions{})

				//	This will fail the TC because we are updating route status
				time.Sleep(10 * time.Millisecond)
				mockCtlr.Agent.respChan <- rscUpdateMeta

				config := ResourceConfigRequest{
					ltmConfig:  mockCtlr.resources.getLTMConfigDeepCopy(),
					shareNodes: mockCtlr.shareNodes,
					gtmConfig:  mockCtlr.resources.getGTMConfigCopy(),
				}
				config.reqId = mockCtlr.Controller.enqueueReq(config)
				mockCtlr.Agent.respChan <- rscUpdateMeta

				mockCtlr.Agent.respChan <- rscUpdateMeta

				time.Sleep(10 * time.Millisecond)

			})
		})
	})
})
