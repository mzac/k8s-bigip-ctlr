package controller

import (
	"context"
	"fmt"
	cisapiv1 "github.com/F5Networks/k8s-bigip-ctlr/v2/config/apis/cis/v1"
	"github.com/F5Networks/k8s-bigip-ctlr/v2/pkg/resource"
	"github.com/F5Networks/k8s-bigip-ctlr/v2/pkg/teem"
	"github.com/F5Networks/k8s-bigip-ctlr/v2/pkg/test"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	routeapi "github.com/openshift/api/route/v1"
	fakeRouteClient "github.com/openshift/client-go/route/clientset/versioned/fake"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"strings"
	"time"
)

var _ = Describe("Routes", func() {
	var mockCtlr *mockController
	BeforeEach(func() {
		mockCtlr = newMockController()
		mockCtlr.mode = OpenShiftMode
		mockCtlr.routeClientV1 = fakeRouteClient.NewSimpleClientset().RouteV1()
		mockCtlr.namespaces = make(map[string]bool)
		mockCtlr.namespaces["default"] = true
		mockCtlr.kubeClient = k8sfake.NewSimpleClientset()
		mockCtlr.nrInformers = make(map[string]*NRInformer)
		mockCtlr.comInformers = make(map[string]*CommonInformer)
		mockCtlr.nativeResourceSelector, _ = createLabelSelector(DefaultNativeResourceLabel)
		mockCtlr.nrInformers["default"] = mockCtlr.newNamespacedNativeResourceInformer("default")
		mockCtlr.nrInformers["test"] = mockCtlr.newNamespacedNativeResourceInformer("test")
		mockCtlr.comInformers["test"] = mockCtlr.newNamespacedCommonResourceInformer("test")
		mockCtlr.comInformers["default"] = mockCtlr.newNamespacedCommonResourceInformer("default")
		var processedHostPath ProcessedHostPath
		processedHostPath.processedHostPathMap = make(map[string]metav1.Time)
		mockCtlr.processedHostPath = &processedHostPath
		mockCtlr.TeemData = &teem.TeemsData{
			ResourceType: teem.ResourceTypes{
				RouteGroups:  make(map[string]int),
				NativeRoutes: make(map[string]int),
				ExternalDNS:  make(map[string]int),
			},
		}
		mockCtlr.Agent = &Agent{
			PostManager: &PostManager{
				PostParams: PostParams{
					BIGIPURL: "10.10.10.1",
				},
			},
		}
	})

	Describe("Routes", func() {
		var rt *routeapi.Route
		var ns string
		BeforeEach(func() {
			ns = "default"
			rt = test.NewRoute(
				"sampleroute",
				"v1",
				ns,
				routeapi.RouteSpec{
					Host: "foo.com",
					Path: "/bar",
					To: routeapi.RouteTargetReference{
						Name: "samplesvc",
					},
				},
				nil,
			)
		})

		It("Base Route", func() {
			mockCtlr.mockResources[ns] = []interface{}{rt}
			mockCtlr.resources = NewResourceStore()
			var override = false
			mockCtlr.resources.extdSpecMap[ns] = &extendedParsedSpec{
				override: override,
				global: &ExtendedRouteGroupSpec{
					VServerName:   "samplevs",
					VServerAddr:   "10.10.10.10",
					AllowOverride: "false",
				},
			}
			err := mockCtlr.processRoutes(ns, false)
			Expect(err).To(BeNil(), "Failed to process routes")
		})
		It("Passthrough Route", func() {
			mockCtlr.mockResources[ns] = []interface{}{rt}
			mockCtlr.resources = NewResourceStore()
			var override = false
			mockCtlr.resources.extdSpecMap[ns] = &extendedParsedSpec{
				override: override,
				global: &ExtendedRouteGroupSpec{
					VServerName:   "samplevs",
					VServerAddr:   "10.10.10.10",
					AllowOverride: "False",
				},
				namespaces: []string{ns},
				partition:  "test",
			}
			tlsConfig := &routeapi.TLSConfig{}
			tlsConfig.Termination = TLSPassthrough
			spec1 := routeapi.RouteSpec{
				Host: "foo.com",
				Path: "/foo",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "foo",
				},
				TLS: tlsConfig,
			}
			route1 := test.NewRoute("route1", "1", ns, spec1, nil)
			mockCtlr.addRoute(route1)
			fooPorts := []v1.ServicePort{{Port: 80, NodePort: 30001},
				{Port: 8080, NodePort: 38001},
				{Port: 9090, NodePort: 39001}}
			foo := test.NewService("foo", "1", ns, "NodePort", fooPorts)
			mockCtlr.addService(foo)
			fooIps := []string{"10.1.1.1"}
			fooEndpts := test.NewEndpoints(
				"foo", "1", "node0", ns, fooIps, []string{},
				convertSvcPortsToEndpointPorts(fooPorts))
			mockCtlr.addEndpoints(fooEndpts)
			mockCtlr.resources.invertedNamespaceLabelMap[ns] = ns

			err := mockCtlr.processRoutes(ns, false)
			mapKey := NameRef{
				Name:      getRSCfgResName("samplevs_443", PassthroughHostsDgName),
				Partition: "test",
			}
			Expect(err).To(BeNil(), "Failed to process routes")
			Expect(len(mockCtlr.resources.ltmConfig["test"].ResourceMap["samplevs_443"].Policies)).To(BeEquivalentTo(0), "Policy should not be created for passthrough route")
			dg, ok := mockCtlr.resources.ltmConfig["test"].ResourceMap["samplevs_443"].IntDgMap[mapKey]
			Expect(ok).To(BeTrue(), "datagroup should be created for passthrough route")
			Expect(dg[ns].Records[0].Name).To(BeEquivalentTo("foo.com"), "Invalid vsHostname in datagroup")
			Expect(dg[ns].Records[0].Data).To(BeEquivalentTo("foo_80_default"), "Invalid vsHostname in datagroup")
		})

		It("Route Admit Status", func() {
			spec1 := routeapi.RouteSpec{
				Host: "foo.com",
				Path: "/foo",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "foo",
				},
			}
			route1 := test.NewRoute("route1", "1", "default", spec1, nil)
			mockCtlr.addRoute(route1)
			rskey := fmt.Sprintf("%v/%v", route1.Namespace, route1.Name)
			mockCtlr.updateRouteAdmitStatus(rskey, "", "", v1.ConditionTrue)
			route := mockCtlr.fetchRoute(rskey)
			Expect(route.Status.Ingress[0].RouterName).To(BeEquivalentTo(F5RouterName), "Incorrect router name")
			Expect(route.Status.Ingress[0].Conditions[0].Status).To(BeEquivalentTo(v1.ConditionTrue), "Incorrect route admit status")
			// Update the status for route with duplicate host path
			mockCtlr.updateRouteAdmitStatus(rskey, "HostAlreadyClaimed", "Testing", v1.ConditionFalse)
			route = mockCtlr.fetchRoute(rskey)
			Expect(route.Status.Ingress[0].Conditions[0].Status).To(BeEquivalentTo(v1.ConditionFalse), "Incorrect route admit status")
			Expect(route.Status.Ingress[0].Conditions[0].Reason).To(BeEquivalentTo("HostAlreadyClaimed"), "Incorrect route admit reason")
			Expect(route.Status.Ingress[0].Conditions[0].Message).To(BeEquivalentTo("Testing"), "Incorrect route admit message")
			//fetch invalid route
			Expect(mockCtlr.fetchRoute(fmt.Sprintf("%v-invalid", rskey))).To(BeNil(), "We should not be able to fetch the route")

		})
		It("Erase All Route Admit Status", func() {
			spec1 := routeapi.RouteSpec{
				Host: "foo.com",
				Path: "/foo",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "foo",
				},
			}
			route1 := test.NewRoute("route1", "1", "default", spec1, nil)
			route1.ObjectMeta.Labels = map[string]string{
				"pro": "asb",
			}
			mockCtlr.addRoute(route1)
			mockCtlr.namespaces = map[string]bool{
				"test": true,
			}
			rskey := fmt.Sprintf("%v/%v", route1.Namespace, route1.Name)
			mockCtlr.updateRouteAdmitStatus(rskey, "Route Admitted", "", v1.ConditionTrue)
			route := mockCtlr.fetchRoute(rskey)
			Expect(len(route1.Status.Ingress)).To(BeEquivalentTo(1), "Incorrect route admit status")
			mockCtlr.routeClientV1.Routes("default").Create(context.TODO(), route1, metav1.CreateOptions{})
			mockCtlr.routeLabel = " pro in (pro) "
			mockCtlr.processedHostPath.processedHostPathMap["foo.com/foo"] = route1.ObjectMeta.CreationTimestamp
			mockCtlr.eraseAllRouteAdmitStatus()
			route = mockCtlr.fetchRoute(rskey)
			Expect(len(route.Status.Ingress)).To(BeEquivalentTo(0), "Incorrect route admit status")
		})
		It("Check Valid Route", func() {
			var cm *v1.ConfigMap
			var data map[string]string
			cmName := "escm"
			cmNamespace := "system"
			mockCtlr.routeSpecCMKey = cmNamespace + "/" + cmName
			mockCtlr.resources = NewResourceStore()
			data = make(map[string]string)
			cm = test.NewConfigMap(
				cmName,
				"v1",
				cmNamespace,
				data)
			data["extendedSpec"] = `
		baseRouteSpec:
		   tlsCipher:
		     tlsVersion : 1.2
		extendedRouteSpec:
		   - namespace: default
		     vserverAddr: 10.8.3.11
		     vserverName: nextgenroutes
		     allowOverride: true
		   - namespace: new
		     vserverAddr: 10.8.3.12
		     allowOverride: true
		`
			_, _ = mockCtlr.processConfigMap(cm, false)

			spec1 := routeapi.RouteSpec{
				Host: "foo.com",
				Path: "/foo",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "foo",
				},
				TLS: &routeapi.TLSConfig{Termination: TLSReencrypt,
					Certificate:                   "",
					Key:                           "",
					InsecureEdgeTerminationPolicy: "",
					DestinationCACertificate:      "",
				},
			}
			spec2 := routeapi.RouteSpec{
				Host: "bar.com",
				Path: "/bar",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "bar",
				},
			}
			spec3 := routeapi.RouteSpec{
				Host: "default.com",
				Path: "/test",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "bar",
				},
				TLS: &routeapi.TLSConfig{Termination: TLSEdge,
					Certificate: "-----BEGIN CERTIFICATE-----\nMIIC+DCCAeCgAwIBAgIQIBIcC6PuJQEHwwI0Hv5QmTANBgkqhkiG9w0BAQsFADAS\nMRAwDgYDVQQKEwdBY21lIENvMB4XDTIyMTIyMjA5MjE0OFoXDTIzMTIyMjA5MjE0\nOFowEjEQMA4GA1UEChMHQWNtZSBDbzCCASIwDQYJKoZIhvcNAQEBBQADggEPADCC\nAQoCggEBAN0NWXsUvGYBV9uo2Iuz3gnovyk3W7p8AA4I8eRUFaWV1EYaxFpsGmdN\nrQgdVJ6w+POSykbDuZynYJyBjC11dJmfTaXffLaUSrJfu+a0QaeWIpt+XxzO4SKQ\nunUSh5Z9w4P45G8VKF7E67wFVN0ni10FLAfBUjYVsQpPagpkH8OdnYCsymCzVSWi\nYETZZ+Hbaih9flRgBQOsoUyNBSkCdJ2wEkZ/0p9+tYwZp1Xvp/Neu3TTsezpu7lE\nbTp0RLQNqfLHWiMV9BSAQRbXAvtvky3J42iy+ec24JyQPtiD85u8Pp/+ssV0ZL9l\nc5KoDEuAvf4NPFWu270gYyQljKcTbB8CAwEAAaNKMEgwDgYDVR0PAQH/BAQDAgWg\nMBMGA1UdJQQMMAoGCCsGAQUFBwMBMAwGA1UdEwEB/wQCMAAwEwYDVR0RBAwwCoII\ndGVzdC5jb20wDQYJKoZIhvcNAQELBQADggEBAI9VUdpVmfx+WUEejREa+plEjCIV\ns+d7v66ddyU4B+Zer1y4RgoWaVq5pywPPjBNJuz6NfwSvBCmuMUd1LUoF5tQFkqb\nVa85Aq6ODbwIMoQ53kTG9vLbT78qESrbukaW9v+axdD9/DIXZJtdwvLvHAVpelRi\n7z48Lxk1GTe7dM3ixKQrU4hz656kH3kXSnD79metOkJA6BAXsqL2XonIhNkCkQVV\n38IHDNkzk228d97ebLu+EhLlkjFgFQEnXusK1amrGJrRDli72pY01yxzGI1caKG5\nN6I8MEIqYI/POwbYWENqONF22pzw/OIs4T1a3jjUqEFugnELcTtx/xRLmOI=\n-----END CERTIFICATE-----\n",
					Key:         "-----BEGIN PRIVATE KEY-----\nMIIEvAIBADANBgkqhkiG9w0BAQEFAASCBKYwggSiAgEAAoIBAQDdDVl7FLxmAVfb\nqNiLs94J6L8pN1u6fAAOCPHkVBWlldRGGsRabBpnTa0IHVSesPjzkspGw7mcp2Cc\ngYwtdXSZn02l33y2lEqyX7vmtEGnliKbfl8czuEikLp1EoeWfcOD+ORvFShexOu8\nBVTdJ4tdBSwHwVI2FbEKT2oKZB/DnZ2ArMpgs1UlomBE2Wfh22oofX5UYAUDrKFM\njQUpAnSdsBJGf9KffrWMGadV76fzXrt007Hs6bu5RG06dES0Danyx1ojFfQUgEEW\n1wL7b5MtyeNosvnnNuCckD7Yg/ObvD6f/rLFdGS/ZXOSqAxLgL3+DTxVrtu9IGMk\nJYynE2wfAgMBAAECggEAf8l91vcvylAweB1twaUjUNsp1yvXbUDNz09Adtxc/zJU\nWoqSxCsGQH3Y7331Mx/fav+Ky8nN/U+NPCxv2r+xvjUncCJ4OBwV6nQJbd76rWTP\ncNBnL4IxCAheodsqYsclRZ+WftjeU5rHJBR48Lgxin6462rImdeEVw99n7At5Kig\nGZmGNXnk6jgvoNU1YJZxSMWQQwKtrfJxXry5a90SfjiviGseuBPsgbrMxEPaeqlQ\nGAMi4nIVRmijL56vbbuuudZm+6dpOnbGzzF6J4M5Nrfr/qJF7ClwXjcMeb6lESIo\n5pmGl3QwSGQYeflFexP3ydvQdUwN5rLbtCexPC2CsQKBgQDxLPn8pIU7WuFiTuOp\n1o7/25v7ijPydIRBjjVeA7E7+mbq9FllkT4CW+HtP7zCCjdScuXhKjuPRrST4fsZ\nZex2nUYfc586s/W95b4QMKtXcJd1MMMWOK2/ZGN/6L5zLPupDrhyWHw91biFZG8h\nSFgn7G2zS/+09gJTglpdj3gClQKBgQDqo7f+kZiXGFvP4kcOWNgnOJOpdqzG/zeD\nuVP2Y6Q8mi7GhkiYhdlrl6Ibh9X0qjFMKMKy827jbUPSGaj5tIT8iXyFT4KVaqZQ\n7r2cMyCqbznKfWlyMyspaVEDa910+VwC2hYQvahTQzfdQqFp6JfiLqCdQtiNDGLf\nbvUOHk4a4wKBgHDLo0NowrMm5wBuewXExm6djE9RrMf5fJ2YYBdPTMYLb7T1gRYC\nnujFhl3KkIKD+qnB+QedE+wHmo8Lgr+3LqevGMu+7LqszgL5fzHdQVWM4Bk8LBGp\ngoFf9zUsal49rJm9u8Am6DyXR0yD04HSbwCFEC1qHvbIk//wmEjnv64dAoGANBbW\nYPBHlLt2nmbYaWn1ync36LYM0zyTQW3iIt+p9T4xRidHdHy6cLU/6qa0K9Wgjgy6\ndGmwY1K9bKX/qjeWEk4fU6T8E1mSxILLmyKKjOuWQ8qlnxGW8mGL95t5lV9KOuPZ\nZCwGcz2H6FnDZbSaCz9YrrDJTD7EsF98jX7SzgsCgYBQv5yi7aGxH6OcrAJPQH4v\n1fZo7mFbqp0WoUMpwuWKNOHZuZoF0EU/bllMZT7AipxVhso+hUC+rDEO7H36TAyc\nTUJbdxtlIC1JmJTmeBOWh3i3Htu8A97DLUNTqNikNyKyGWjy7eC0ncG3+CGG91wA\nky9KxzxszaIez6kIUCY7xQ==\n-----END PRIVATE KEY-----\n",
				},
			}
			spec4 := routeapi.RouteSpec{
				Host: "test.com",
				Path: "/",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "bar",
				},
				TLS: &routeapi.TLSConfig{Termination: TLSReencrypt},
			}
			annotations := make(map[string]string)
			annotations[resource.F5ClientSslProfileAnnotation] = "/Common/clientssl"

			route1 := test.NewRoute("route1", "1", "default", spec1, annotations)
			route2 := test.NewRoute("route2", "1", "test", spec1, nil)
			route3 := test.NewRoute("route3", "1", "default", spec2, nil)
			route4 := test.NewRoute("route4", "1", "default", spec3, nil)
			route5 := test.NewRoute("route5", "1", "default", spec4, nil)
			mockCtlr.addRoute(route1)
			mockCtlr.addRoute(route2)
			mockCtlr.addRoute(route3)
			mockCtlr.addRoute(route4)
			_, _ = mockCtlr.processConfigMap(cm, false)
			mockCtlr.addRoute(route5)
			mockCtlr.routeClientV1.Routes("default").Create(context.TODO(), route1, metav1.CreateOptions{})
			mockCtlr.routeClientV1.Routes("default").Create(context.TODO(), route2, metav1.CreateOptions{})
			mockCtlr.routeClientV1.Routes("default").Create(context.TODO(), route3, metav1.CreateOptions{})
			mockCtlr.routeClientV1.Routes("default").Create(context.TODO(), route4, metav1.CreateOptions{})
			mockCtlr.routeClientV1.Routes("default").Create(context.TODO(), route5, metav1.CreateOptions{})
			rskey1 := fmt.Sprintf("%v/%v", route1.Namespace, route1.Name)
			rskey2 := fmt.Sprintf("%v/%v", route2.Namespace, route2.Name)
			rskey3 := fmt.Sprintf("%v/%v", route3.Namespace, route3.Name)
			Expect(mockCtlr.checkValidRoute(route1)).To(BeFalse())
			mockCtlr.processedHostPath.processedHostPathMap[route1.Spec.Host+route1.Spec.Path] = route1.ObjectMeta.CreationTimestamp
			Expect(mockCtlr.checkValidRoute(route2)).To(BeFalse())
			Expect(mockCtlr.checkValidRoute(route3)).To(BeFalse())
			Expect(mockCtlr.checkValidRoute(route4)).To(BeFalse())
			mockCtlr.resources.baseRouteConfig.DefaultTLS = DefaultSSLProfile{Reference: BIGIP}
			Expect(mockCtlr.checkValidRoute(route5)).To(BeFalse())
			mockCtlr.resources.baseRouteConfig.DefaultTLS = DefaultSSLProfile{Reference: BIGIP, ClientSSL: "/Common/clientSSL"}
			Expect(mockCtlr.checkValidRoute(route5)).To(BeFalse())
			mockCtlr.resources.baseRouteConfig.DefaultTLS = DefaultSSLProfile{}
			Expect(mockCtlr.checkValidRoute(route5)).To(BeFalse())
			time.Sleep(100 * time.Millisecond)
			route1 = mockCtlr.fetchRoute(rskey1)
			route2 = mockCtlr.fetchRoute(rskey2)
			route3 = mockCtlr.fetchRoute(rskey3)
			Expect(route1.Status.Ingress[0].RouterName).To(BeEquivalentTo(F5RouterName), "Incorrect router name")
			Expect(route2.Status.Ingress[0].RouterName).To(BeEquivalentTo(F5RouterName), "Incorrect router name")
			Expect(route1.Status.Ingress[0].Conditions[0].Status).To(BeEquivalentTo(v1.ConditionFalse), "Incorrect route admit status")
			Expect(route2.Status.Ingress[0].Conditions[0].Status).To(BeEquivalentTo(v1.ConditionFalse), "Incorrect route admit status")
			Expect(route1.Status.Ingress[0].Conditions[0].Reason).To(BeEquivalentTo("ExtendedValidationFailed"), "Incorrect route admit reason")
			Expect(route2.Status.Ingress[0].Conditions[0].Reason).To(BeEquivalentTo("HostAlreadyClaimed"), "incorrect the route admit reason")
			// checkValidRoute should fail with ServiceNotFound error
			Expect(route3.Status.Ingress[0].RouterName).To(BeEquivalentTo(F5RouterName), "Incorrect router name")
			Expect(route3.Status.Ingress[0].Conditions[0].Status).To(BeEquivalentTo(v1.ConditionFalse), "Incorrect route admit status")
			Expect(route3.Status.Ingress[0].Conditions[0].Reason).To(BeEquivalentTo("ServiceNotFound"), "Incorrect route admit reason")
		})
		It("Check GSLB Support for Routes", func() {
			var cm *v1.ConfigMap
			var data map[string]string
			cmName := "escm"
			cmNamespace := "kube-system"
			mockCtlr.routeSpecCMKey = cmNamespace + "/" + cmName
			mockCtlr.resources = NewResourceStore()
			data = make(map[string]string)
			mockCtlr.Partition = "default"
			cm = test.NewConfigMap(
				cmName,
				"v1",
				cmNamespace,
				data)
			data["extendedSpec"] = `
baseRouteSpec: 
    tlsCipher:
      tlsVersion : 1.2
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: true
    - namespace: test
      vserverAddr: 10.8.3.12
      allowOverride: true
      bigIpPartition: dev
`
			err, isProcessed := mockCtlr.processConfigMap(cm, false)
			Expect(err).To(BeNil())
			Expect(isProcessed).To(BeTrue())

			namespace1 := "default"
			namespace2 := "test"
			spec1 := routeapi.RouteSpec{
				Host: "pytest-foo-1.com",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "foo",
				},
				TLS: &routeapi.TLSConfig{Termination: "edge"},
			}
			spec2 := routeapi.RouteSpec{
				Host: "pytest-bar-1.com",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "bar",
				},
				TLS: &routeapi.TLSConfig{Termination: "edge"},
			}
			fooPorts := []v1.ServicePort{{Port: 80, NodePort: 30001},
				{Port: 8080, NodePort: 38001},
				{Port: 9090, NodePort: 39001}}
			foo := test.NewService("foo", "1", namespace1, "NodePort", fooPorts)
			mockCtlr.addService(foo)
			fooIps := []string{"10.1.1.1"}
			fooEndpts := test.NewEndpoints(
				"foo", "1", "node0", namespace1, fooIps, []string{},
				convertSvcPortsToEndpointPorts(fooPorts))
			mockCtlr.addEndpoints(fooEndpts)

			//Add new Route
			annotation1 := make(map[string]string)
			annotation1[resource.F5ServerSslProfileAnnotation] = "/Common/serverssl"
			annotation1[resource.F5ClientSslProfileAnnotation] = "/Common/clientssl"
			route1 := test.NewRoute("route1", "1", namespace1, spec1, annotation1)
			mockCtlr.addRoute(route1)
			mockCtlr.resources.invertedNamespaceLabelMap[namespace1] = namespace1
			err = mockCtlr.processRoutes(namespace1, false)

			bar := test.NewService("bar", "1", namespace2, "NodePort", fooPorts)
			mockCtlr.addService(bar)
			barIPs := []string{"10.1.1.1"}
			barEndpts := test.NewEndpoints(
				"bar", "1", "node0", namespace2, barIPs, []string{},
				convertSvcPortsToEndpointPorts(fooPorts))
			mockCtlr.addEndpoints(barEndpts)

			//Add new Route
			annotation2 := make(map[string]string)
			annotation2[resource.F5ServerSslProfileAnnotation] = "/Common/serverssl"
			annotation2[resource.F5ClientSslProfileAnnotation] = "/Common/clientssl"
			route2 := test.NewRoute("route2", "1", namespace2, spec2, annotation2)
			mockCtlr.addRoute(route2)
			mockCtlr.resources.invertedNamespaceLabelMap[namespace2] = namespace2
			err = mockCtlr.processRoutes(namespace2, false)

			newEDNS := test.NewExternalDNS(
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
			//Process ENDS with non-matching domain
			mockCtlr.addEDNS(newEDNS)
			mockCtlr.processExternalDNS(newEDNS, false)
			gtmConfig := mockCtlr.resources.gtmConfig[DEFAULT_PARTITION].WideIPs
			Expect(len(gtmConfig)).To(Equal(1))
			Expect(len(gtmConfig["test.com"].Pools)).To(Equal(1))
			// No pool member should be present
			Expect(len(gtmConfig["test.com"].Pools[0].Members)).To(Equal(0))

			//delete EDNS
			mockCtlr.deleteEDNS(newEDNS)
			mockCtlr.processExternalDNS(newEDNS, true)
			gtmConfig = mockCtlr.resources.gtmConfig[DEFAULT_PARTITION].WideIPs
			Expect(len(gtmConfig)).To(Equal(0))

			// Modify EDNS with matching domain and create again
			mockCtlr.addEDNS(newEDNS)
			newEDNS.Spec.DomainName = "pytest-foo-1.com"
			mockCtlr.processExternalDNS(newEDNS, false)
			gtmConfig = mockCtlr.resources.gtmConfig[DEFAULT_PARTITION].WideIPs
			Expect(len(gtmConfig)).To(Equal(1))
			Expect(len(gtmConfig["pytest-foo-1.com"].Pools)).To(Equal(1))
			// Pool member should be present
			Expect(len(gtmConfig["pytest-foo-1.com"].Pools[0].Members)).To(Equal(1))

			// Delete domain matching route
			mockCtlr.deleteRoute(route1)
			mockCtlr.deleteHostPathMapEntry(route1)
			mockCtlr.processRoutes(namespace1, false)
			gtmConfig = mockCtlr.resources.gtmConfig[DEFAULT_PARTITION].WideIPs
			Expect(len(gtmConfig)).To(Equal(1))
			Expect(len(gtmConfig["pytest-foo-1.com"].Pools)).To(Equal(1))
			// No pool member should be present
			Expect(len(gtmConfig["pytest-foo-1.com"].Pools[0].Members)).To(Equal(0))

			// Recreate route
			mockCtlr.addRoute(route1)
			mockCtlr.resources.invertedNamespaceLabelMap[namespace1] = namespace1
			err = mockCtlr.processRoutes(namespace1, false)
			Expect(len(gtmConfig)).To(Equal(1))
			Expect(len(gtmConfig["pytest-foo-1.com"].Pools)).To(Equal(1))
			Expect(len(gtmConfig["pytest-foo-1.com"].Pools[0].Members)).To(Equal(1))

			//Update route host
			route1.Spec.Host = "pytest-foo-2.com"
			mockCtlr.updateRoute(route1)
			var key string
			if route1.Spec.Path == "/" || len(route1.Spec.Path) == 0 {
				key = route1.Spec.Host + "/"
			} else {
				key = route1.Spec.Host + route1.Spec.Path
			}
			mockCtlr.updateHostPathMap(route1.ObjectMeta.CreationTimestamp, key)
			err = mockCtlr.processRoutes(namespace1, false)
			Expect(len(gtmConfig)).To(Equal(1))
			Expect(len(gtmConfig["pytest-foo-1.com"].Pools)).To(Equal(1))
			// There should be no pool members
			Expect(len(gtmConfig["pytest-foo-1.com"].Pools[0].Members)).To(Equal(0))

			//Reset host
			route1.Spec.Host = "pytest-foo-1.com"
			mockCtlr.updateRoute(route1)
			err = mockCtlr.processRoutes(namespace1, false)

			barEDNS := test.NewExternalDNS(
				"barEDNS",
				"default",
				cisapiv1.ExternalDNSSpec{
					DomainName: "pytest-bar-1.com",
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
			//Test with 2nd route with bigIpPartition
			mockCtlr.addEDNS(barEDNS)
			mockCtlr.processExternalDNS(barEDNS, false)
			gtmConfig = mockCtlr.resources.gtmConfig[DEFAULT_PARTITION].WideIPs
			Expect(len(gtmConfig)).To(Equal(2))
			Expect(len(gtmConfig["pytest-bar-1.com"].Pools)).To(Equal(1))
			Expect(len(gtmConfig["pytest-bar-1.com"].Pools[0].Members)).To(Equal(1))
			Expect(strings.Contains(gtmConfig["pytest-bar-1.com"].Pools[0].Members[0], "routes_10.8_3_12_dev"))

			mockCtlr.deleteEDNS(barEDNS)
			mockCtlr.processExternalDNS(barEDNS, true)
			gtmConfig = mockCtlr.resources.gtmConfig[DEFAULT_PARTITION].WideIPs
			Expect(len(gtmConfig)).To(Equal(1))

			//Remove route group
			data["extendedSpec"] = `
baseRouteSpec: 
    tlsCipher:
      tlsVersion : 1.2
extendedRouteSpec:
    - namespace: test
      vserverAddr: 10.8.3.12
      allowOverride: true
`
			err, isProcessed = mockCtlr.processConfigMap(cm, false)
			Expect(err).To(BeNil())
			Expect(isProcessed).To(BeTrue())

			gtmConfig = mockCtlr.resources.gtmConfig[DEFAULT_PARTITION].WideIPs
			Expect(len(gtmConfig)).To(Equal(1))
			Expect(len(gtmConfig["pytest-foo-1.com"].Pools)).To(Equal(1))
			//No pool members should present
			Expect(len(gtmConfig["pytest-foo-1.com"].Pools[0].Members)).To(Equal(0))

		})
		It("Check Host-Path Map functions", func() {
			spec1 := routeapi.RouteSpec{
				Host: "foo.com",
				Path: "/foo",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "foo",
				},
				TLS: &routeapi.TLSConfig{Termination: "edge",
					Certificate:                   "",
					Key:                           "",
					InsecureEdgeTerminationPolicy: "",
					DestinationCACertificate:      "",
				},
			}
			route1 := test.NewRoute("route1", "1", "default", spec1, nil)
			mockCtlr.addRoute(route1)
			// test hostpathMap update function
			oldURI := route1.Spec.Host + route1.Spec.Path
			route1.Spec.Path = "/test"
			newURI := route1.Spec.Host + route1.Spec.Path
			mockCtlr.updateRoute(route1)
			mockCtlr.updateHostPathMap(route1.ObjectMeta.CreationTimestamp, route1.Spec.Host+route1.Spec.Path)
			_, found := mockCtlr.processedHostPath.processedHostPathMap[oldURI]
			Expect(found).To(BeFalse())
			_, found = mockCtlr.processedHostPath.processedHostPathMap[newURI]
			Expect(found).To(BeTrue())
			Expect(len(mockCtlr.processedHostPath.processedHostPathMap)).To(BeEquivalentTo(1))
			mockCtlr.deleteRoute(route1)
		})
		It("Checks whether Forwarding policy is added correctly", func() {
			routeGroup := "default"
			spec1 := routeapi.RouteSpec{
				Host: "foo.com",
				Path: "/foo",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "foo",
				},
				TLS: &routeapi.TLSConfig{Termination: "edge",
					Certificate:                   "",
					Key:                           "",
					InsecureEdgeTerminationPolicy: "",
					DestinationCACertificate:      "",
				},
			}
			spec2 := routeapi.RouteSpec{
				Host: "bar.com",
				Path: "/bar",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "bar",
				},
				TLS: &routeapi.TLSConfig{Termination: "edge",
					Certificate:                   "",
					Key:                           "",
					InsecureEdgeTerminationPolicy: "",
					DestinationCACertificate:      "",
				},
			}
			route1 := test.NewRoute("route1", "1", routeGroup, spec1, nil)
			route2 := test.NewRoute("route2", "1", routeGroup, spec2, nil)

			// Resource Config for unsecured virtual server
			rsCfg := &ResourceConfig{}
			rsCfg.Virtual.Partition = routeGroup
			rsCfg.MetaData.ResourceType = VirtualServer
			rsCfg.Virtual.Enabled = true
			rsCfg.Virtual.Name = "newroutes_80"
			rsCfg.MetaData.Protocol = HTTP
			rsCfg.Virtual.SetVirtualAddress("10.8.3.11", DEFAULT_HTTP_PORT)
			// Portstruct for unsecured virtual server
			ps := portStruct{HTTP, DEFAULT_HTTP_PORT}
			// HTTP virtual server, secured route, InsecureEdgeTerminationPolicy = ""
			Expect(mockCtlr.prepareResourceConfigFromRoute(rsCfg, route1, intstr.IntOrString{IntVal: 80}, ps)).To(BeNil())
			Expect(rsCfg.Policies).To(BeNil())
			// HTTP virtual server, secured route, InsecureEdgeTerminationPolicy = "None"
			route1.Spec.TLS.InsecureEdgeTerminationPolicy = routeapi.InsecureEdgeTerminationPolicyNone
			Expect(mockCtlr.prepareResourceConfigFromRoute(rsCfg, route1, intstr.IntOrString{IntVal: 80}, ps)).To(BeNil())
			Expect(rsCfg.Policies).To(BeNil())
			// HTTP virtual server, secured route, InsecureEdgeTerminationPolicy = "Allow"
			route1.Spec.TLS.InsecureEdgeTerminationPolicy = routeapi.InsecureEdgeTerminationPolicyAllow
			Expect(mockCtlr.prepareResourceConfigFromRoute(rsCfg, route1, intstr.IntOrString{IntVal: 80}, ps)).To(BeNil())
			Expect(rsCfg.Policies).NotTo(BeNil())
			Expect(len(rsCfg.Policies)).To(Equal(1))
			Expect(len(rsCfg.Policies[0].Rules)).To(Equal(1))
			// HTTP virtual server, secured route, InsecureEdgeTerminationPolicy = ""
			Expect(mockCtlr.prepareResourceConfigFromRoute(rsCfg, route2, intstr.IntOrString{IntVal: 80}, ps)).To(BeNil())
			Expect(rsCfg.Policies).NotTo(BeNil())
			Expect(len(rsCfg.Policies)).To(Equal(1))
			Expect(len(rsCfg.Policies[0].Rules)).To(Equal(1))
			Expect(rsCfg.Policies[0].Rules[0].FullURI).To(Equal("foo.com/foo"))

			// ResourceConfig for secured virtual server
			rsCfg = &ResourceConfig{}
			rsCfg.Virtual.Partition = routeGroup
			rsCfg.MetaData.ResourceType = VirtualServer
			rsCfg.Virtual.Enabled = true
			rsCfg.Virtual.Name = "newroutes_443"
			rsCfg.MetaData.Protocol = HTTPS
			rsCfg.Virtual.SetVirtualAddress("10.8.3.11", DEFAULT_HTTPS_PORT)
			// Portstruct for secured virtual server
			ps.protocol = HTTPS
			ps.port = DEFAULT_HTTPS_PORT
			Expect(mockCtlr.prepareResourceConfigFromRoute(rsCfg, route1, intstr.IntOrString{IntVal: 80}, ps)).To(BeNil())
			Expect(rsCfg.Policies).NotTo(BeNil())
			Expect(len(rsCfg.Policies)).To(Equal(1))
			Expect(len(rsCfg.Policies[0].Rules)).To(Equal(1))
			Expect(rsCfg.Policies[0].Rules[0].FullURI).To(Equal("foo.com/foo"))
			Expect(mockCtlr.prepareResourceConfigFromRoute(rsCfg, route2, intstr.IntOrString{IntVal: 80}, ps)).To(BeNil())
			Expect(rsCfg.Policies).NotTo(BeNil())
			Expect(len(rsCfg.Policies)).To(Equal(1))
			Expect(len(rsCfg.Policies[0].Rules)).To(Equal(2))
			Expect(rsCfg.Policies[0].Rules[0].FullURI).To(Equal("foo.com/foo"))
			Expect(rsCfg.Policies[0].Rules[1].FullURI).To(Equal("bar.com/bar"))

		})

		It("Check Route A/B Deploy", func() {
			routeGroup := "default"
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

			spec1 := routeapi.RouteSpec{
				Host: "pytest-foo-1.com",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "foo",
				},
				TLS: &routeapi.TLSConfig{Termination: "reencrypt",
					Certificate:              " -----BEGIN CERTIFICATE-----\n      MIIDDjCCAfYCCQCgR208hrCAozANBgkqhkiG9w0BAQsFADBCMQswCQYDVQQGEwJV\n      UzELMAkGA1UECAwCQ08xDDAKBgNVBAcMA0JETzELMAkGA1UECwwCY2ExCzAJBgNV\n      BAoMAkY1MB4XDTIyMTAxODA4MDg0NVoXDTIyMTAyMTA4MDg0NVowUDELMAkGA1UE\n      BhMCVVMxCzAJBgNVBAgMAkNPMQwwCgYDVQQHDANCRE8xCzAJBgNVBAoMAkY1MRkw\n      FwYDVQQDDBBweXRlc3QtZm9vLTEuY29tMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A\n      MIIBCgKCAQEAy3IHmdvGjR/fSti25e4YKpotbwkG/WOcOkXk+IwJuu14c/4dsDM1\n      7IayBOWuyhxvQUTyIpmNNqkb1PJ1cY1+6eIdecXdFhUPZtKylxE6NhqWtxpYn1jU\n      byiH1iqKS899MjbQ9GUrfBy/SZxwEkupq/WJcdvbtuYClUgMXqAcLpDQFZoPCWn9\n      qkFj3BubkQp2trO+2K4VGURTNixDcSZs+GoTpZQSS1E6KFAFWu8T9WgnWODWZi1D\n      OGoYb0+rgso9qi1FgPNSPbEqgi82917rUobC8qK8TweXL0xq4rgpAv3Ypsc4Mhbx\n      cm9Gh1QflH+MDI3eqYhN9F5oMQYYeH3HKwIDAQABMA0GCSqGSIb3DQEBCwUAA4IB\n      AQCvmFvTHeY5x0MMYR99DmkxwgTKE5yRgUs5X276quCUL/XJezOmLXYmWeuKy3U8\n      Z4L1zkGj4saH9ysZ1PwhHgrBIIfJpsMMFyvA8CHlO0bCBk4q/5vLAGDlsVj6UXx4\n      VZupUmJbapBXIM20WSqDeM6PVlbsBO1t8tJPV+NOYOS+M8muXlotivKUrB2zwggS\n      7+VMgWgJ6Rq4+uPVL+LOYUEY31pUkhUFnxdw9iSwuLiFIT6B9QtwVydXqe82X6KM\n      ncH6TIRTYXmTXy9CU3YqJWGl6E4Bybr6Uzlkyoo1CEKDetbwBrgrEwr8Cs8i/K4C\n      rTbQUqAOMjosET4jarlY9/t0\n      -----END CERTIFICATE-----",
					DestinationCACertificate: " -----BEGIN CERTIFICATE-----\n      MIIDVzCCAj+gAwIBAgIJAPl2S8PFsPkeMA0GCSqGSIb3DQEBCwUAMEIxCzAJBgNV\n      BAYTAlVTMQswCQYDVQQIDAJDTzEMMAoGA1UEBwwDQkRPMQswCQYDVQQLDAJjYTEL\n      MAkGA1UECgwCRjUwHhcNMjIwOTIzMTYxMzMxWhcNMjMwOTIzMTYxMzMxWjBCMQsw\n      CQYDVQQGEwJVUzELMAkGA1UECAwCQ08xDDAKBgNVBAcMA0JETzELMAkGA1UECwwC\n      Y2ExCzAJBgNVBAoMAkY1MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA\n      0luQ/n3iC/3kA3RAYveM1hpXsOThcyzb8xT8QoL58i+J2P/pGl+8Ho2HHS+4+jbG\n      7iJ5m46yflLWSLXqSVtvIuEgXDFr8bkLGhuUYZfMQyprzSUN+QNM6EtHsrXSeJGE\n      /qOSOPPm7M2eJoS+DDhiAaTiOAAd2iUJ4bCrsc4RBRuZaXx4Gxcmdk5fqwt5Urqc\n      iNDteplu3UJ4TibP/dTkqEqZ7o6E2kUxzIBjtZqG09cqrygX/ayZYYjuFzl6Ksyp\n      5dUC0TZ2RZwfd7564xBOdCAKcHDgbm7ygP8MMJa2olj04f0d5hikJcFAxVhWePzL\n      BxpDjXNfv6shzshL0StXJQIDAQABo1AwTjAdBgNVHQ4EFgQUdGNL5SrEZp+ukaR/\n      lIken7t1um4wHwYDVR0jBBgwFoAUdGNL5SrEZp+ukaR/lIken7t1um4wDAYDVR0T\n      BAUwAwEB/zANBgkqhkiG9w0BAQsFAAOCAQEAaO8ZWa/94FvAW8ZcSLooEchWw98G\n      7IK4+nzLo+b4GEKhV9ALH0Cz6+UUW3+9v56kdHTIgDOR7lF5lPyzPTEh4PgpiX8M\n      rmtzqEM3CBJEGNuAaSk4vxNCTVX3vLBqMG53VmWFPuqHqoa46VIV/HzSQVBjJu6x\n      JfjKRDEvsgGSSrv6W/x5getsjIO0SQuuMVH4IJuD3oQWvf5WfYZMf+53ToHSRncy\n      2kiQtgbsxK/KWDix9TM+hhkILFvU/CmpTTweD8hNpCOvF5GLs9lhMVBFc+HJBVtZ\n      qfVuJiZMiyIyaGbxefgz60QgCBuLcyaAVafRH7rSRr43DNP0Pm2k4figzg==\n      -----END CERTIFICATE-----",
					Key:                      "-----BEGIN RSA PRIVATE KEY-----\n      MIIEpAIBAAKCAQEAy3IHmdvGjR/fSti25e4YKpotbwkG/WOcOkXk+IwJuu14c/4d\n      sDM17IayBOWuyhxvQUTyIpmNNqkb1PJ1cY1+6eIdecXdFhUPZtKylxE6NhqWtxpY\n      n1jUbyiH1iqKS899MjbQ9GUrfBy/SZxwEkupq/WJcdvbtuYClUgMXqAcLpDQFZoP\n      CWn9qkFj3BubkQp2trO+2K4VGURTNixDcSZs+GoTpZQSS1E6KFAFWu8T9WgnWODW\n      Zi1DOGoYb0+rgso9qi1FgPNSPbEqgi82917rUobC8qK8TweXL0xq4rgpAv3Ypsc4\n      Mhbxcm9Gh1QflH+MDI3eqYhN9F5oMQYYeH3HKwIDAQABAoIBACLPujk7f/f58i1O\n      c81YNk5j305Wjxmgh8T43Lsiyy9vHuNKIi5aNOnqCmAIJSZ0Qx05/OyqtZ0axqZj\n      bnElswe2JzEFCFWU+POxLdnnmrxTRGLEYVGy03bJyqR81vkt4dBLzOlkvlIYYSrp\n      V8vponjIJOKUqj3bkamVkHhIkUnuM2lXdC30VcWBU5m9S6SuwjNFOLzhrIucXATA\n      vvKH+Bw6tGKI5yE8PkSyW8BCnFg24AF2UQq1k8XvjnT3CTVeCxEZUp+HOt1Y2F25\n      AhqE0viC2KeJtG0y34QKhbxq5gtUljbNCaKUkKJlO4Hu+bGVrZGPmAIEMPwMgX9u\n      JaH2w/ECgYEA63XUA243qlMESfasD2BbIxyO6Wqk47CGZvfj6N66pFQO075Vv3dO\n      IY1ENT/Cd73XE9zxr/9RQ4BG42pWL1/3g1jcpAa+iW2SK1YxaCe3SwSQY+EWuGsY\n      XmhahZ/V7aD5PH4v+ewOG1r6WF5ugwoaaEvn/9/f3At4TszX9/acWbcCgYEA3TFD\n      blSk+iFWjXnYzTTgS+5ZVt2c3Ix4iEY1pCRpcMsCbqx0BiqjXUCtHBDNQ5+LxlyD\n      wLMjcQGGIyfSlLxuXQONRRfo2PZjcYe7JvxsX/FrXTvFi0n+i9o2HM38nH2Un40Z\n      cpr/fpcpvC8kFD20jo/nt8J8OdZT9fZ5WIa2Di0CgYBQQW8sZCrxES7LDxsCerNV\n      umwzvzfIq+iDvEagnxo63LPZFG0hv8aPxRjUlZDxQ3HFwW9Xr8zBFz4SUbJin3E8\n      AdPizLGxIfnKb6yTdcYR+dJFWPlnjolV1HfWR+6g+lc5eUFdDEqapF3kNPuyCoWJ\n      uyWun14sIHS3Vzbdu9767QKBgQDQiTB0pXLAq4upaFYA6bgJflZWMitAN2Mvv1m1\n      Per2vz60zvu4EJziPya1zhVnitTBl9lTZNCmKvSm0lWTiq9WHBIlMOyDGJAaqgfF\n      MriOH9LEHKUatBE7EuhvcbiWZUMoxWNXjFASrjtXwu3181L2ETA6LC7obGvN+ajf\n      0Gl1pQKBgQCAzIzP5ab8vvqwHVhDN+mWfG3vvN3tCI2rL4zv5boO20MqVTxu9i7o\n      e7Zro8EKG/HNmt7hF46vq2OJa5QUpNf6a1II4dRsbbBoFUzGinm41TUENkeMumTU\n      XsGWrknaI+J90tmvkM8rSI1Qjcw1zHUWTyd7blDj/snjb/Qg4v57yw==\n      -----END RSA PRIVATE KEY-----"},
			}
			fooPorts := []v1.ServicePort{{Port: 80, NodePort: 30001},
				{Port: 8080, NodePort: 38001},
				{Port: 9090, NodePort: 39001}}
			foo := test.NewService("foo", "1", routeGroup, "NodePort", fooPorts)
			mockCtlr.addService(foo)
			fooIps := []string{"10.1.1.1"}
			fooEndpts := test.NewEndpoints(
				"foo", "1", "node0", routeGroup, fooIps, []string{},
				convertSvcPortsToEndpointPorts(fooPorts))
			mockCtlr.addEndpoints(fooEndpts)
			//Domain Based Route
			annotation1 := make(map[string]string)
			annotation1[resource.F5ServerSslProfileAnnotation] = "/Common/serverssl"
			annotation1[resource.F5ClientSslProfileAnnotation] = "/Common/clientssl"
			route1 := test.NewRoute("route1", "1", routeGroup, spec1, annotation1)

			mockCtlr.addRoute(route1)
			mockCtlr.resources.invertedNamespaceLabelMap[routeGroup] = routeGroup
			err := mockCtlr.processRoutes(routeGroup, false)
			parition := mockCtlr.resources.extdSpecMap[routeGroup].partition
			vsName := frameRouteVSName(mockCtlr.resources.extdSpecMap[routeGroup].global.VServerName, mockCtlr.resources.extdSpecMap[routeGroup].global.VServerAddr, portStruct{protocol: "https", port: 443})
			Expect(err).To(BeNil())
			Expect(len(mockCtlr.resources.ltmConfig[parition].ResourceMap[vsName].IRulesMap) == 1).To(BeTrue())

			var alternateBackend []routeapi.RouteTargetReference
			weight := new(int32)
			*weight = 50
			alternateBackend = append(alternateBackend, routeapi.RouteTargetReference{Kind: "Service",
				Name: "foo", Weight: weight})

			spec1.AlternateBackends = alternateBackend
			//Domain based route with alternate backend
			route2 := test.NewRoute("route2", "1", routeGroup, spec1, annotation1)

			mockCtlr.addRoute(route2)
			mockCtlr.resources.invertedNamespaceLabelMap[routeGroup] = routeGroup
			err = mockCtlr.processRoutes(routeGroup, false)
			Expect(err).To(BeNil())
			Expect(len(mockCtlr.resources.ltmConfig[parition].ResourceMap[vsName].IRulesMap) == 1).To(BeTrue())

			spec2 := routeapi.RouteSpec{
				Host: "pytest-foo-1.com",
				Path: "/first",
				To: routeapi.RouteTargetReference{
					Kind:   "Service",
					Name:   "foo",
					Weight: weight,
				},
				AlternateBackends: alternateBackend,
				TLS: &routeapi.TLSConfig{Termination: "reencrypt",
					Certificate:              " -----BEGIN CERTIFICATE-----\n      MIIDDjCCAfYCCQCgR208hrCAozANBgkqhkiG9w0BAQsFADBCMQswCQYDVQQGEwJV\n      UzELMAkGA1UECAwCQ08xDDAKBgNVBAcMA0JETzELMAkGA1UECwwCY2ExCzAJBgNV\n      BAoMAkY1MB4XDTIyMTAxODA4MDg0NVoXDTIyMTAyMTA4MDg0NVowUDELMAkGA1UE\n      BhMCVVMxCzAJBgNVBAgMAkNPMQwwCgYDVQQHDANCRE8xCzAJBgNVBAoMAkY1MRkw\n      FwYDVQQDDBBweXRlc3QtZm9vLTEuY29tMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A\n      MIIBCgKCAQEAy3IHmdvGjR/fSti25e4YKpotbwkG/WOcOkXk+IwJuu14c/4dsDM1\n      7IayBOWuyhxvQUTyIpmNNqkb1PJ1cY1+6eIdecXdFhUPZtKylxE6NhqWtxpYn1jU\n      byiH1iqKS899MjbQ9GUrfBy/SZxwEkupq/WJcdvbtuYClUgMXqAcLpDQFZoPCWn9\n      qkFj3BubkQp2trO+2K4VGURTNixDcSZs+GoTpZQSS1E6KFAFWu8T9WgnWODWZi1D\n      OGoYb0+rgso9qi1FgPNSPbEqgi82917rUobC8qK8TweXL0xq4rgpAv3Ypsc4Mhbx\n      cm9Gh1QflH+MDI3eqYhN9F5oMQYYeH3HKwIDAQABMA0GCSqGSIb3DQEBCwUAA4IB\n      AQCvmFvTHeY5x0MMYR99DmkxwgTKE5yRgUs5X276quCUL/XJezOmLXYmWeuKy3U8\n      Z4L1zkGj4saH9ysZ1PwhHgrBIIfJpsMMFyvA8CHlO0bCBk4q/5vLAGDlsVj6UXx4\n      VZupUmJbapBXIM20WSqDeM6PVlbsBO1t8tJPV+NOYOS+M8muXlotivKUrB2zwggS\n      7+VMgWgJ6Rq4+uPVL+LOYUEY31pUkhUFnxdw9iSwuLiFIT6B9QtwVydXqe82X6KM\n      ncH6TIRTYXmTXy9CU3YqJWGl6E4Bybr6Uzlkyoo1CEKDetbwBrgrEwr8Cs8i/K4C\n      rTbQUqAOMjosET4jarlY9/t0\n      -----END CERTIFICATE-----",
					DestinationCACertificate: " -----BEGIN CERTIFICATE-----\n      MIIDVzCCAj+gAwIBAgIJAPl2S8PFsPkeMA0GCSqGSIb3DQEBCwUAMEIxCzAJBgNV\n      BAYTAlVTMQswCQYDVQQIDAJDTzEMMAoGA1UEBwwDQkRPMQswCQYDVQQLDAJjYTEL\n      MAkGA1UECgwCRjUwHhcNMjIwOTIzMTYxMzMxWhcNMjMwOTIzMTYxMzMxWjBCMQsw\n      CQYDVQQGEwJVUzELMAkGA1UECAwCQ08xDDAKBgNVBAcMA0JETzELMAkGA1UECwwC\n      Y2ExCzAJBgNVBAoMAkY1MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA\n      0luQ/n3iC/3kA3RAYveM1hpXsOThcyzb8xT8QoL58i+J2P/pGl+8Ho2HHS+4+jbG\n      7iJ5m46yflLWSLXqSVtvIuEgXDFr8bkLGhuUYZfMQyprzSUN+QNM6EtHsrXSeJGE\n      /qOSOPPm7M2eJoS+DDhiAaTiOAAd2iUJ4bCrsc4RBRuZaXx4Gxcmdk5fqwt5Urqc\n      iNDteplu3UJ4TibP/dTkqEqZ7o6E2kUxzIBjtZqG09cqrygX/ayZYYjuFzl6Ksyp\n      5dUC0TZ2RZwfd7564xBOdCAKcHDgbm7ygP8MMJa2olj04f0d5hikJcFAxVhWePzL\n      BxpDjXNfv6shzshL0StXJQIDAQABo1AwTjAdBgNVHQ4EFgQUdGNL5SrEZp+ukaR/\n      lIken7t1um4wHwYDVR0jBBgwFoAUdGNL5SrEZp+ukaR/lIken7t1um4wDAYDVR0T\n      BAUwAwEB/zANBgkqhkiG9w0BAQsFAAOCAQEAaO8ZWa/94FvAW8ZcSLooEchWw98G\n      7IK4+nzLo+b4GEKhV9ALH0Cz6+UUW3+9v56kdHTIgDOR7lF5lPyzPTEh4PgpiX8M\n      rmtzqEM3CBJEGNuAaSk4vxNCTVX3vLBqMG53VmWFPuqHqoa46VIV/HzSQVBjJu6x\n      JfjKRDEvsgGSSrv6W/x5getsjIO0SQuuMVH4IJuD3oQWvf5WfYZMf+53ToHSRncy\n      2kiQtgbsxK/KWDix9TM+hhkILFvU/CmpTTweD8hNpCOvF5GLs9lhMVBFc+HJBVtZ\n      qfVuJiZMiyIyaGbxefgz60QgCBuLcyaAVafRH7rSRr43DNP0Pm2k4figzg==\n      -----END CERTIFICATE-----",
					Key:                      "-----BEGIN RSA PRIVATE KEY-----\n      MIIEpAIBAAKCAQEAy3IHmdvGjR/fSti25e4YKpotbwkG/WOcOkXk+IwJuu14c/4d\n      sDM17IayBOWuyhxvQUTyIpmNNqkb1PJ1cY1+6eIdecXdFhUPZtKylxE6NhqWtxpY\n      n1jUbyiH1iqKS899MjbQ9GUrfBy/SZxwEkupq/WJcdvbtuYClUgMXqAcLpDQFZoP\n      CWn9qkFj3BubkQp2trO+2K4VGURTNixDcSZs+GoTpZQSS1E6KFAFWu8T9WgnWODW\n      Zi1DOGoYb0+rgso9qi1FgPNSPbEqgi82917rUobC8qK8TweXL0xq4rgpAv3Ypsc4\n      Mhbxcm9Gh1QflH+MDI3eqYhN9F5oMQYYeH3HKwIDAQABAoIBACLPujk7f/f58i1O\n      c81YNk5j305Wjxmgh8T43Lsiyy9vHuNKIi5aNOnqCmAIJSZ0Qx05/OyqtZ0axqZj\n      bnElswe2JzEFCFWU+POxLdnnmrxTRGLEYVGy03bJyqR81vkt4dBLzOlkvlIYYSrp\n      V8vponjIJOKUqj3bkamVkHhIkUnuM2lXdC30VcWBU5m9S6SuwjNFOLzhrIucXATA\n      vvKH+Bw6tGKI5yE8PkSyW8BCnFg24AF2UQq1k8XvjnT3CTVeCxEZUp+HOt1Y2F25\n      AhqE0viC2KeJtG0y34QKhbxq5gtUljbNCaKUkKJlO4Hu+bGVrZGPmAIEMPwMgX9u\n      JaH2w/ECgYEA63XUA243qlMESfasD2BbIxyO6Wqk47CGZvfj6N66pFQO075Vv3dO\n      IY1ENT/Cd73XE9zxr/9RQ4BG42pWL1/3g1jcpAa+iW2SK1YxaCe3SwSQY+EWuGsY\n      XmhahZ/V7aD5PH4v+ewOG1r6WF5ugwoaaEvn/9/f3At4TszX9/acWbcCgYEA3TFD\n      blSk+iFWjXnYzTTgS+5ZVt2c3Ix4iEY1pCRpcMsCbqx0BiqjXUCtHBDNQ5+LxlyD\n      wLMjcQGGIyfSlLxuXQONRRfo2PZjcYe7JvxsX/FrXTvFi0n+i9o2HM38nH2Un40Z\n      cpr/fpcpvC8kFD20jo/nt8J8OdZT9fZ5WIa2Di0CgYBQQW8sZCrxES7LDxsCerNV\n      umwzvzfIq+iDvEagnxo63LPZFG0hv8aPxRjUlZDxQ3HFwW9Xr8zBFz4SUbJin3E8\n      AdPizLGxIfnKb6yTdcYR+dJFWPlnjolV1HfWR+6g+lc5eUFdDEqapF3kNPuyCoWJ\n      uyWun14sIHS3Vzbdu9767QKBgQDQiTB0pXLAq4upaFYA6bgJflZWMitAN2Mvv1m1\n      Per2vz60zvu4EJziPya1zhVnitTBl9lTZNCmKvSm0lWTiq9WHBIlMOyDGJAaqgfF\n      MriOH9LEHKUatBE7EuhvcbiWZUMoxWNXjFASrjtXwu3181L2ETA6LC7obGvN+ajf\n      0Gl1pQKBgQCAzIzP5ab8vvqwHVhDN+mWfG3vvN3tCI2rL4zv5boO20MqVTxu9i7o\n      e7Zro8EKG/HNmt7hF46vq2OJa5QUpNf6a1II4dRsbbBoFUzGinm41TUENkeMumTU\n      XsGWrknaI+J90tmvkM8rSI1Qjcw1zHUWTyd7blDj/snjb/Qg4v57yw==\n      -----END RSA PRIVATE KEY-----"},
			}

			route3 := test.NewRoute("route3", "1", routeGroup, spec2, annotation1)
			mockCtlr.addRoute(route3)
			err = mockCtlr.processRoutes(routeGroup, false)
			Expect(err).To(BeNil())

			abPathIRule := getRSCfgResName(vsName, ABPathIRuleName)
			Expect(len(mockCtlr.resources.ltmConfig["test"].ResourceMap[vsName].IRulesMap) == 2).To(BeTrue())
			Expect(mockCtlr.resources.ltmConfig["test"].ResourceMap[vsName].IRulesMap[NameRef{abPathIRule, parition}].Name == abPathIRule).To(BeTrue())

		})

		It("Check Route TLS", func() {

			annotation1 := make(map[string]string)
			annotation1[resource.F5ServerSslProfileAnnotation] = "/Common/serverssl"
			annotation1[resource.F5ClientSslProfileAnnotation] = "/Common/clientssl"

			clientSSLAnnotation := make(map[string]string)
			clientSSLAnnotation[resource.F5ClientSslProfileAnnotation] = "/Common/clientssl"

			serverSSLAnnotation := make(map[string]string)
			serverSSLAnnotation[resource.F5ServerSslProfileAnnotation] = "/Common/serverssl"

			extdSpec := &ExtendedRouteGroupSpec{
				VServerName:   "defaultServer",
				VServerAddr:   "10.8.3.11",
				AllowOverride: "0",
			}

			//with no tls defined
			extdSpec1 := &ExtendedRouteGroupSpec{
				VServerName:   "defaultServer",
				VServerAddr:   "10.8.3.11",
				AllowOverride: "0",
			}

			// with only client tls defined
			extdSpec2 := &ExtendedRouteGroupSpec{
				VServerName:   "defaultServer",
				VServerAddr:   "10.8.3.11",
				AllowOverride: "0",
			}

			spec1 := routeapi.RouteSpec{
				Host: "foo.com",
				Path: "/foo",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "foo",
				},
				TLS: &routeapi.TLSConfig{Termination: "edge"},
			}

			spec2 := routeapi.RouteSpec{
				Host: "bar.com",
				Path: "/bar",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "bar",
				},
				TLS: &routeapi.TLSConfig{Termination: "reencrypt"},
			}

			routeGroup := "default"

			route1 := test.NewRoute("route1", "1", routeGroup, spec1, annotation1)
			route2 := test.NewRoute("route2", "2", routeGroup, spec2, annotation1)
			rsCfg := &ResourceConfig{}
			rsCfg.Virtual.Partition = routeGroup
			rsCfg.MetaData.ResourceType = VirtualServer
			rsCfg.Virtual.Enabled = true
			rsCfg.Virtual.Name = "newroutes_443"
			rsCfg.MetaData.Protocol = HTTPS
			rsCfg.Virtual.SetVirtualAddress("10.8.3.11", DEFAULT_HTTPS_PORT)
			ps := portStruct{HTTP, DEFAULT_HTTP_PORT}
			// Portstruct for secured virtual server
			ps.protocol = HTTPS
			ps.port = DEFAULT_HTTPS_PORT
			rsCfg.IntDgMap = make(InternalDataGroupMap)
			rsCfg.IRulesMap = make(IRulesMap)
			Expect(mockCtlr.prepareResourceConfigFromRoute(rsCfg, route1, intstr.IntOrString{IntVal: 443}, ps)).To(BeNil())

			//for edge route, and big ip reference in global config map - It should pass
			Expect(mockCtlr.handleRouteTLS(
				rsCfg,
				route1,
				extdSpec.VServerAddr,
				intstr.IntOrString{IntVal: 443})).To(BeTrue())

			//for edge route and global config map without client ssl profile - It should fail
			route1.Annotations = serverSSLAnnotation
			Expect(mockCtlr.handleRouteTLS(
				rsCfg,
				route1,
				extdSpec1.VServerAddr,
				intstr.IntOrString{IntVal: 443})).To(BeFalse())

			//for re-encrypt route, and big ip reference in global config map - It should pass
			route2.Annotations = annotation1
			Expect(mockCtlr.handleRouteTLS(
				rsCfg,
				route2,
				extdSpec.VServerAddr,
				intstr.IntOrString{IntVal: 443})).To(BeTrue())

			//for re encrypt route and global config map without server ssl profile - It should fail
			route2.Annotations = clientSSLAnnotation
			Expect(mockCtlr.handleRouteTLS(
				rsCfg,
				route2,
				extdSpec2.VServerAddr,
				intstr.IntOrString{IntVal: 443})).To(BeFalse())
		})

		It("Verify NextGenRoutes K8S Secret as TLS certs", func() {
			mockCtlr.resources = &ResourceStore{
				supplementContextCache: supplementContextCache{
					baseRouteConfig: BaseRouteConfig{
						TLSCipher{
							"1.2",
							"DEFAULT",
							"/Common/f5-default",
						},
						DefaultSSLProfile{},
						DefaultRouteGroupConfig{},
					},
				},
			}
			namespace := "default"
			data := make(map[string][]byte)
			data["tls.key"] = []byte{}
			data["tls.crt"] = []byte{}
			clientssl := &v1.Secret{
				TypeMeta:   metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{Name: "clientssl", Namespace: namespace},
				Data:       data,
			}
			serverssl := &v1.Secret{
				TypeMeta:   metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{Name: "serverssl", Namespace: namespace},
				Data:       data,
			}
			mockCtlr.comInformers["default"].secretsInformer.GetStore().Add(clientssl)
			mockCtlr.comInformers["default"].secretsInformer.GetStore().Add(serverssl)

			annotation1 := make(map[string]string)
			annotation1[resource.F5ServerSslProfileAnnotation] = "serverssl"
			annotation1[resource.F5ClientSslProfileAnnotation] = "clientssl"

			clientSSLAnnotation := make(map[string]string)
			clientSSLAnnotation[resource.F5ClientSslProfileAnnotation] = "clientssl"

			serverSSLAnnotation := make(map[string]string)
			serverSSLAnnotation[resource.F5ServerSslProfileAnnotation] = "serverssl"

			extdSpec := &ExtendedRouteGroupSpec{
				VServerName:   "defaultServer",
				VServerAddr:   "10.8.3.11",
				AllowOverride: "0",
			}

			//with no tls defined
			extdSpec1 := &ExtendedRouteGroupSpec{
				VServerName:   "defaultServer",
				VServerAddr:   "10.8.3.11",
				AllowOverride: "0",
			}

			// with only client tls defined
			extdSpec2 := &ExtendedRouteGroupSpec{
				VServerName:   "defaultServer",
				VServerAddr:   "10.8.3.11",
				AllowOverride: "0",
			}

			spec1 := routeapi.RouteSpec{
				Host: "foo.com",
				Path: "/foo",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "foo",
				},
				TLS: &routeapi.TLSConfig{Termination: "edge"},
			}

			spec2 := routeapi.RouteSpec{
				Host: "bar.com",
				Path: "/bar",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "bar",
				},
				TLS: &routeapi.TLSConfig{Termination: "reencrypt"},
			}

			routeGroup := "default"

			route1 := test.NewRoute("route1", "1", routeGroup, spec1, annotation1)
			route2 := test.NewRoute("route2", "2", routeGroup, spec2, annotation1)
			rsCfg := &ResourceConfig{}
			rsCfg.Virtual.Partition = routeGroup
			rsCfg.MetaData.ResourceType = VirtualServer
			rsCfg.Virtual.Enabled = true
			rsCfg.Virtual.Name = "newroutes_443"
			rsCfg.MetaData.Protocol = HTTPS
			rsCfg.Virtual.SetVirtualAddress("10.8.3.11", DEFAULT_HTTPS_PORT)
			ps := portStruct{HTTP, DEFAULT_HTTP_PORT}
			// Portstruct for secured virtual server
			ps.protocol = HTTPS
			ps.port = DEFAULT_HTTPS_PORT
			rsCfg.IntDgMap = make(InternalDataGroupMap)
			rsCfg.IRulesMap = make(IRulesMap)
			rsCfg.customProfiles = make(map[SecretKey]CustomProfile)
			Expect(mockCtlr.prepareResourceConfigFromRoute(rsCfg, route1, intstr.IntOrString{IntVal: 443}, ps)).To(BeNil())

			//for edge route, and k8s secret as TLS certs in global config map - It should pass
			Expect(mockCtlr.handleRouteTLS(
				rsCfg,
				route1,
				extdSpec.VServerAddr,
				intstr.IntOrString{IntVal: 443})).To(BeTrue())

			//for edge route and global config map without client ssl profile - It should fail
			route1.Annotations = serverSSLAnnotation
			Expect(mockCtlr.handleRouteTLS(
				rsCfg,
				route1,
				extdSpec1.VServerAddr,
				intstr.IntOrString{IntVal: 443})).To(BeFalse())

			//for re-encrypt route, and k8s secret as TLS certs in global config map - It should pass
			route2.Annotations = annotation1
			Expect(mockCtlr.handleRouteTLS(
				rsCfg,
				route2,
				extdSpec.VServerAddr,
				intstr.IntOrString{IntVal: 443})).To(BeTrue())

			//for re encrypt route and global config map without server ssl profile - It should fail
			route2.Annotations = clientSSLAnnotation
			Expect(mockCtlr.handleRouteTLS(
				rsCfg,
				route2,
				extdSpec2.VServerAddr,
				intstr.IntOrString{IntVal: 443})).To(BeFalse())

			// Verify that getRouteGroupForSecret fetches the z routeGroup on k8s secret update
			// Prepare extdSpecMap that holds all the
			mockCtlr.resources.extdSpecMap = make(map[string]*extendedParsedSpec)
			mockCtlr.resources.extdSpecMap[routeGroup] = &extendedParsedSpec{
				global: &ExtendedRouteGroupSpec{VServerName: "default"},
			}
			mockCtlr.resources.extdSpecMap["test1"] = &extendedParsedSpec{
				global: &ExtendedRouteGroupSpec{VServerName: "test1"},
			}
			mockCtlr.resources.extdSpecMap["test2"] = &extendedParsedSpec{
				global: &ExtendedRouteGroupSpec{VServerName: "test2"},
			}
			// Prepare invertedNamespaceLabelMap that maps namespaces to routeGroup
			mockCtlr.resources.invertedNamespaceLabelMap = make(map[string]string)
			mockCtlr.resources.invertedNamespaceLabelMap[routeGroup] = routeGroup
			mockCtlr.resources.invertedNamespaceLabelMap["test2"] = routeGroup
			mockCtlr.resources.invertedNamespaceLabelMap["test1"] = "test1"
			// get routeGroup clientssl secret which belongs to default namespace
			Expect(mockCtlr.getRouteGroupForSecret(clientssl)).To(Equal(routeGroup))
			// get routeGroup clientssl secret which belongs to test3 namespace
			Expect(mockCtlr.getRouteGroupForSecret(&v1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "clientssl",
				Namespace: "test3"}})).To(Equal(""))
			// Needs to be handled
			// get routeGroup clientssl1 secret which belongs to default namespace
			//Expect(mockCtlr.getRouteGroupForSecret(&v1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "clientssl1",
			//	Namespace: "default"}})).To(Equal(""))

		})
		It("Verify Routes with Different scenarios", func() {
			ports := []portStruct{
				{
					protocol: "http",
					port:     DEFAULT_HTTP_PORT,
				},
			}
			secureRoute := *rt
			secureRoute.Spec.TLS = &routeapi.TLSConfig{}
			secureRoute.Spec.TLS.Termination = TLSReencrypt
			secureRoute.Spec.TLS.InsecureEdgeTerminationPolicy = routeapi.InsecureEdgeTerminationPolicyAllow

			mockCtlr.addRoute(rt)
			rsKey := fmt.Sprintf("%v/%v", "test1", rt.Name)
			route := mockCtlr.fetchRoute(rsKey)
			//Invalid Namespace
			Expect(route).To(BeNil())

			rsKey = fmt.Sprintf("%v/%v", "test", "")
			route = mockCtlr.fetchRoute(rsKey)
			Expect(route).To(BeNil())

			rsKey = fmt.Sprintf("%v/%v", rt.Namespace, rt.Name)
			route = mockCtlr.fetchRoute(rsKey)
			Expect(route).ToNot(BeNil())
			Expect(mockCtlr.GetHostFromHostPath(rt.Spec.Host)).To(Equal("foo.com"))
			Expect(isPassthroughRoute(rt)).To(BeFalse())
			Expect(doRoutesHandleHTTP([]*routeapi.Route{rt})).To(BeTrue())
			Expect(doRoutesHandleHTTP([]*routeapi.Route{&secureRoute})).To(BeTrue())
			secureRoute.Namespace = "test1"
			mockCtlr.getServicePort(&secureRoute)

			// Invalid Route Key
			mockCtlr.eraseRouteAdmitStatus("test")
			// Invalid Route Key
			mockCtlr.updateRouteAdmitStatus("test", "", "", v1.ConditionTrue)
			Expect(getVirtualPortsForRoutes([]*routeapi.Route{rt})).To(Equal(ports))
			secureRoute.Namespace = "test"
			mockCtlr.getServicePort(&secureRoute)

			rt.Spec.Port = &routeapi.RoutePort{TargetPort: intstr.IntOrString{StrVal: "http"}}
			mockCtlr.getServicePort(rt)
		})

	})

	Describe("Extended Spec ConfigMap", func() {
		var cm *v1.ConfigMap
		var data map[string]string
		BeforeEach(func() {
			cmName := "escm"
			cmNamespace := "system"
			mockCtlr.routeSpecCMKey = cmNamespace + "/" + cmName
			mockCtlr.resources = NewResourceStore()
			data = make(map[string]string)
			cm = test.NewConfigMap(
				cmName,
				"v1",
				cmNamespace,
				data)
		})

		It("Extended Route Spec Global", func() {
			data["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: true
    - namespace: new
      vserverAddr: 10.8.3.12
      allowOverride: true
`
			err, ok := mockCtlr.processConfigMap(cm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())

			data["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: invalid
    - namespace: new
      vserverAddr: 10.8.3.12
      allowOverride: true
`
			err, ok = mockCtlr.processConfigMap(cm, false)
			Expect(err).ToNot(BeNil(), "invalid allowOverride value")
			Expect(ok).To(BeFalse())

			data["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: true
    - namespace: new
      vserverAddr: 10.8.3.12
      allowOverride: true
      vserverName: newroutes
`
			err, ok = mockCtlr.processConfigMap(cm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())
		})

		It("Extended Route Spec Allow local", func() {
			data["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: true
    - namespace: new
      vserverAddr: 10.8.3.12
      allowOverride: true
`
			err, ok := mockCtlr.processConfigMap(cm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())

			localData := make(map[string]string)
			localCm := test.NewConfigMap(
				"localESCM",
				"v1",
				"default",
				localData)
			localData["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.110
      vserverName: nextgenroutes
`
			err, ok = mockCtlr.processConfigMap(localCm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())
		})

		It("Extended Route Spec Do not Allow local", func() {
			data["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: false
    - namespace: new
      vserverAddr: 10.8.3.12
      allowOverride: false
`
			err, ok := mockCtlr.processConfigMap(cm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())

			localData := make(map[string]string)
			localCm := test.NewConfigMap(
				"localESCM",
				"v1",
				"default",
				localData)
			localData["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.110
      vserverName: nextgenroutes
`
			err, ok = mockCtlr.processConfigMap(localCm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())
		})

		It("Extended Route Spec Allow local Update with out spec change", func() {
			data["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: true
    - namespace: new
      vserverAddr: 10.8.3.12
      allowOverride: true
`

			err, ok := mockCtlr.processConfigMap(cm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())

			localData := make(map[string]string)
			localCm := test.NewConfigMap(
				"localESCM",
				"v1",
				"default",
				localData)
			localData["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.110
      vserverName: nextgenroutes
`
			err, ok = mockCtlr.processConfigMap(localCm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())
			localData["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverName: nextgenroutes
      vserverAddr: 10.8.3.110
`
			err, ok = mockCtlr.processConfigMap(localCm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())
		})

		It("Extended local Route Spec pickup alternate configmap when latest gets deleted", func() {
			data["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      allowOverride: true
`
			namespace := "default"
			err, ok := mockCtlr.processConfigMap(cm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())

			localData := make(map[string]string)
			localCm1 := test.NewConfigMap(
				"localESCM1",
				"v1",
				namespace,
				localData)
			localData["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.110
      vserverName: nextgenroutes
`
			localCm2 := test.NewConfigMap(
				"localESCM2",
				"v1",
				namespace,
				localData)
			localData["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.110
      vserverName: newvs
`
			localCm3 := test.NewConfigMap(
				"localESCM3",
				"v1",
				namespace,
				localData)
			localData["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.110
      vserverName: latestserver
`

			_ = mockCtlr.nrInformers[namespace].cmInformer.GetIndexer().Add(localCm1)
			_ = mockCtlr.nrInformers[namespace].cmInformer.GetIndexer().Add(localCm2)
			_ = mockCtlr.nrInformers[namespace].cmInformer.GetIndexer().Add(localCm3)
			err, ok = mockCtlr.processConfigMap(localCm3, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())

			_ = mockCtlr.nrInformers[namespace].cmInformer.GetIndexer().Delete(localCm3)
			err, ok = mockCtlr.processConfigMap(localCm3, true)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())
			Expect(mockCtlr.resources.extdSpecMap[namespace].local.VServerName).To(Equal("latestserver"), "Spec from wrong configmap")
		})

		It("Operational Specs on configmap Create/Update/Delete events", func() {
			cachedExtdSpecMap := make(map[string]*extendedParsedSpec)
			newExtdSpecMap := make(map[string]*extendedParsedSpec)

			newExtdSpecMap["default"] = &extendedParsedSpec{
				override: false,
				local:    nil,
				global: &ExtendedRouteGroupSpec{
					VServerName:   "defaultServer",
					VServerAddr:   "10.8.3.11",
					AllowOverride: "0",
				},
			}
			newExtdSpecMap["new"] = &extendedParsedSpec{
				override: false,
				local:    nil,
				global: &ExtendedRouteGroupSpec{
					VServerName:   "newServer",
					VServerAddr:   "10.8.3.12",
					AllowOverride: "f",
				},
			}
			deletedSpecs, modifiedSpecs, updatedSpecs, createdSpecs := getOperationalExtendedConfigMapSpecs(
				cachedExtdSpecMap, newExtdSpecMap, false,
			)
			Expect(len(deletedSpecs)).To(BeZero())
			Expect(len(modifiedSpecs)).To(BeZero())
			Expect(len(updatedSpecs)).To(BeZero())
			Expect(len(createdSpecs)).To(Equal(2))

			cachedExtdSpecMap["default"] = &extendedParsedSpec{
				override: false,
				local:    nil,
				global: &ExtendedRouteGroupSpec{
					VServerName:   "defaultServer",
					VServerAddr:   "10.8.3.11",
					AllowOverride: "false",
				},
			}
			cachedExtdSpecMap["new"] = &extendedParsedSpec{
				override: false,
				local:    nil,
				global: &ExtendedRouteGroupSpec{
					VServerName:   "newServer",
					VServerAddr:   "10.8.3.12",
					AllowOverride: "FALSE",
				},
			}

			newExtdSpecMap["default"].global.Policy = "test/policy1"
			newExtdSpecMap["new"].global.Policy = "test/policy1"

			deletedSpecs, modifiedSpecs, updatedSpecs, createdSpecs = getOperationalExtendedConfigMapSpecs(
				cachedExtdSpecMap, newExtdSpecMap, false,
			)
			Expect(len(deletedSpecs)).To(BeZero())
			Expect(len(modifiedSpecs)).To(BeZero())
			Expect(len(updatedSpecs)).To(Equal(2))
			Expect(len(createdSpecs)).To(BeZero())

			newExtdSpecMap["default"].global.VServerName = "defaultServer1"
			newExtdSpecMap["new"].global.VServerName = "newServer1"

			deletedSpecs, modifiedSpecs, updatedSpecs, createdSpecs = getOperationalExtendedConfigMapSpecs(
				cachedExtdSpecMap, newExtdSpecMap, false,
			)
			Expect(len(deletedSpecs)).To(BeZero())
			Expect(len(modifiedSpecs)).To(Equal(2))
			Expect(len(updatedSpecs)).To(BeZero())
			Expect(len(createdSpecs)).To(BeZero())

			deletedSpecs, modifiedSpecs, updatedSpecs, createdSpecs = getOperationalExtendedConfigMapSpecs(
				cachedExtdSpecMap, newExtdSpecMap, true,
			)
			Expect(len(deletedSpecs)).To(Equal(2))
			Expect(len(modifiedSpecs)).To(BeZero())
			Expect(len(updatedSpecs)).To(BeZero())
			Expect(len(createdSpecs)).To(BeZero())

			delete(newExtdSpecMap, "new")
			newExtdSpecMap["default"] = &extendedParsedSpec{
				override: false,
				local:    nil,
				global: &ExtendedRouteGroupSpec{
					VServerName:   "defaultServer",
					VServerAddr:   "10.8.3.11",
					AllowOverride: "false",
				},
			}
			deletedSpecs, modifiedSpecs, updatedSpecs, createdSpecs = getOperationalExtendedConfigMapSpecs(
				cachedExtdSpecMap, newExtdSpecMap, false,
			)
			Expect(len(deletedSpecs)).To(Equal(1))
			Expect(len(modifiedSpecs)).To(BeZero())
			Expect(len(updatedSpecs)).To(BeZero())
			Expect(len(createdSpecs)).To(BeZero())
		})

		It("Global ConfigMap with base route config", func() {
			data["extendedSpec"] = `
baseRouteSpec: 
    tlsCipher:
      tlsVersion : 1.2
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: true
    - namespace: new
      vserverAddr: 10.8.3.12
      allowOverride: true
`
			err, ok := mockCtlr.processConfigMap(cm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())

			data["extendedSpec"] = `
baseRouteSpec: 
    tlsCipher:
      tlsVersion : 1.3
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: true
`
			err, ok = mockCtlr.processConfigMap(cm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())

			routeGroup := "default"
			mockCtlr.resources = NewResourceStore()
			mockCtlr.resources.extdSpecMap[routeGroup] = &extendedParsedSpec{
				override: false,
				global: &ExtendedRouteGroupSpec{
					VServerName:   "nextgenroutes",
					VServerAddr:   "10.10.10.10",
					AllowOverride: "False",
				},
				namespaces: []string{routeGroup},
				partition:  "test",
			}

			spec1 := routeapi.RouteSpec{
				Host: "foo.com",
				Path: "/foo",
				To: routeapi.RouteTargetReference{
					Kind: "Service",
					Name: "foo",
				},
				TLS: &routeapi.TLSConfig{Termination: "reencrypt"},
			}
			fooPorts := []v1.ServicePort{{Port: 80, NodePort: 30001},
				{Port: 8080, NodePort: 38001},
				{Port: 9090, NodePort: 39001}}
			foo := test.NewService("foo", "1", routeGroup, "NodePort", fooPorts)
			mockCtlr.addService(foo)
			fooIps := []string{"10.1.1.1"}
			fooEndpts := test.NewEndpoints(
				"foo", "1", "node0", routeGroup, fooIps, []string{},
				convertSvcPortsToEndpointPorts(fooPorts))
			mockCtlr.addEndpoints(fooEndpts)
			annotations := make(map[string]string)
			annotations["virtual-server.f5.com/balance"] = "least-connections-node"
			annotations[resource.F5ServerSslProfileAnnotation] = "/Common/serverssl"
			annotations[resource.F5ClientSslProfileAnnotation] = "/Common/clientssl"
			route1 := test.NewRoute("route1", "1", routeGroup, spec1, annotations)
			mockCtlr.addRoute(route1)
			mockCtlr.resources.invertedNamespaceLabelMap[routeGroup] = routeGroup
			err = mockCtlr.processRoutes(routeGroup, false)

			Expect(err).To(BeNil())
			Expect(mockCtlr.resources.ltmConfig["test"].ResourceMap["nextgenroutes_443"].Pools[0].Balance == "least-connections-node").To(BeTrue())

			data["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: true
`
			err, ok = mockCtlr.processConfigMap(cm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())
		})
	})
})

var _ = Describe("With NamespaceLabel parameter in deployment", func() {
	var mockCtlr *mockController
	BeforeEach(func() {
		mockCtlr = newMockController()
		mockCtlr.mode = OpenShiftMode
		mockCtlr.routeClientV1 = fakeRouteClient.NewSimpleClientset().RouteV1()
		mockCtlr.namespaces = make(map[string]bool)
		mockCtlr.namespaces["default"] = true
		mockCtlr.kubeClient = k8sfake.NewSimpleClientset()
		mockCtlr.nrInformers = make(map[string]*NRInformer)
		mockCtlr.comInformers = make(map[string]*CommonInformer)
		mockCtlr.nsInformers = make(map[string]*NSInformer)
		mockCtlr.nativeResourceSelector, _ = createLabelSelector(DefaultNativeResourceLabel)
		mockCtlr.nrInformers["default"] = mockCtlr.newNamespacedNativeResourceInformer("default")
		mockCtlr.comInformers["default"] = mockCtlr.newNamespacedCommonResourceInformer("default")
		mockCtlr.nrInformers["test"] = mockCtlr.newNamespacedNativeResourceInformer("test")
		mockCtlr.comInformers["test"] = mockCtlr.newNamespacedCommonResourceInformer("test")
		mockCtlr.comInformers["default"] = mockCtlr.newNamespacedCommonResourceInformer("default")
		mockCtlr.namespaceLabel = "environment=dev"
		var processedHostPath ProcessedHostPath
		processedHostPath.processedHostPathMap = make(map[string]metav1.Time)
		mockCtlr.processedHostPath = &processedHostPath
		mockCtlr.TeemData = &teem.TeemsData{
			ResourceType: teem.ResourceTypes{
				RouteGroups:  make(map[string]int),
				NativeRoutes: make(map[string]int),
			},
		}
	})
	Describe("Extended Spec ConfigMap", func() {
		var cm *v1.ConfigMap
		var data map[string]string
		BeforeEach(func() {
			cmName := "escm"
			cmNamespace := "system"
			mockCtlr.routeSpecCMKey = cmNamespace + "/" + cmName
			mockCtlr.resources = NewResourceStore()
			data = make(map[string]string)
			cm = test.NewConfigMap(
				cmName,
				"v1",
				cmNamespace,
				data)
		})

		It("namespace and namespaceLabel combination", func() {
			data["extendedSpec"] = `
extendedRouteSpec:
    - namespace: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: true
      bigIpPartition: foo
    - namespaceLabel: bar=true
      vserverAddr: 10.8.3.12
      allowOverride: true
`
			err := mockCtlr.setNamespaceLabelMode(cm)
			Expect(err).To(MatchError(fmt.Sprintf("can not specify both namespace and namespace-label in extended configmap %v/%v", cm.Namespace, cm.Name)))
		})
		It("with namespaceLabel only", func() {
			data["extendedSpec"] = `
extendedRouteSpec:
    - namespaceLabel: foo=true
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: true
      bigIpPartition: foo
    - namespaceLabel: bar=true
      vserverAddr: 10.8.3.12
      allowOverride: true
`
			mockCtlr.namespaceLabelMode = true
			err, ok := mockCtlr.processConfigMap(cm, false)
			Expect(err).To(BeNil())
			Expect(ok).To(BeTrue())
		})
	})
})

var _ = Describe("Without NamespaceLabel parameter in deployment", func() {
	var mockCtlr *mockController
	BeforeEach(func() {
		mockCtlr = newMockController()
		mockCtlr.mode = OpenShiftMode
	})
	Describe("Extended Spec ConfigMap", func() {
		var cm *v1.ConfigMap
		var data map[string]string
		BeforeEach(func() {
			cmName := "escm"
			cmNamespace := "system"
			mockCtlr.routeSpecCMKey = cmNamespace + "/" + cmName
			mockCtlr.resources = NewResourceStore()
			data = make(map[string]string)
			cm = test.NewConfigMap(
				cmName,
				"v1",
				cmNamespace,
				data)
		})
		It("namespaceLabel only without namespace-label deployment parameter", func() {
			data["extendedSpec"] = `
extendedRouteSpec:
    - namespaceLabel: default
      vserverAddr: 10.8.3.11
      vserverName: nextgenroutes
      allowOverride: true
      bigIpPartition: foo
    - namespaceLabel: bar=true
      vserverAddr: 10.8.3.12
      allowOverride: true
`
			err := mockCtlr.setNamespaceLabelMode(cm)
			Expect(err).To(MatchError("--namespace-label deployment parameter is required with namespace-label in extended configmap"))
		})
	})
})
