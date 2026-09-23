package provider

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func ptr[T any](v T) *T { return &v }

func host(h string) *gatewayv1.Hostname { hh := gatewayv1.Hostname(h); return &hh }

func testClass(name string, controller gatewayv1.GatewayController, gen int64) *gatewayv1.GatewayClass {
	return &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: gen},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: controller},
	}
}

func testGateway(ns, name, class string, gen int64, listeners ...gatewayv1.Listener) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: gen},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(class),
			Listeners:        listeners,
		},
	}
}

func plainListener(name string, port int32, h *gatewayv1.Hostname) gatewayv1.Listener {
	return gatewayv1.Listener{
		Name:     gatewayv1.SectionName(name),
		Hostname: h,
		Port:     gatewayv1.PortNumber(port),
		Protocol: gatewayv1.HTTPProtocolType,
	}
}

func tlsListener(name string, port int32, h *gatewayv1.Hostname, protocol gatewayv1.ProtocolType, certNS *gatewayv1.Namespace, certName string) gatewayv1.Listener {
	return gatewayv1.Listener{
		Name:     gatewayv1.SectionName(name),
		Hostname: h,
		Port:     gatewayv1.PortNumber(port),
		Protocol: protocol,
		TLS: &gatewayv1.ListenerTLSConfig{
			Mode: ptr(gatewayv1.TLSModeTerminate),
			CertificateRefs: []gatewayv1.SecretObjectReference{{
				Name:      gatewayv1.ObjectName(certName),
				Namespace: certNS,
			}},
		},
	}
}

func testRoute(ns, name string, gen int64, hostnames []gatewayv1.Hostname, parents []gatewayv1.ParentReference, rules ...gatewayv1.HTTPRouteRule) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: gen},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parents},
			Hostnames:       hostnames,
			Rules:           rules,
		},
	}
}

func gwParent(name string) gatewayv1.ParentReference {
	return gatewayv1.ParentReference{Name: gatewayv1.ObjectName(name)}
}

func gwParentSection(name, section string) gatewayv1.ParentReference {
	return gatewayv1.ParentReference{
		Name:        gatewayv1.ObjectName(name),
		SectionName: ptr(gatewayv1.SectionName(section)),
	}
}

func gwParentInNS(name, ns string) gatewayv1.ParentReference {
	return gatewayv1.ParentReference{
		Name:      gatewayv1.ObjectName(name),
		Namespace: ptr(gatewayv1.Namespace(ns)),
	}
}

// pathBackendRule builds a rule with one path match and one Service backend.
func pathBackendRule(matchType gatewayv1.PathMatchType, path, svc string, port int32) gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{Type: ptr(matchType), Value: ptr(path)},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: gatewayv1.ObjectName(svc),
					Port: ptr(gatewayv1.PortNumber(port)),
				},
			},
		}},
	}
}

func backendRule(svc string, port int32) gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: gatewayv1.ObjectName(svc),
					Port: ptr(gatewayv1.PortNumber(port)),
				},
			},
		}},
	}
}

func testTLSSecret(ns, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": []byte("-----BEGIN CERTIFICATE-----\ncert\n-----END CERTIFICATE-----\n"),
			"tls.key": []byte("-----BEGIN PRIVATE KEY-----\nkey\n-----END PRIVATE KEY-----\n"),
		},
	}
}

func testSlice(ns, svc string, port int32, ready *bool, addrs ...string) *discoveryv1.EndpointSlice {
	p := port
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      svc + "-slice",
			Labels:    map[string]string{discoveryv1.LabelServiceName: svc},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Port: &p}},
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  addrs,
			Conditions: discoveryv1.EndpointConditions{Ready: ready},
		}},
	}
}

func build(t *testing.T, objs ...any) *Graph {
	t.Helper()
	res := &Resources{}
	for _, o := range objs {
		switch v := o.(type) {
		case *gatewayv1.GatewayClass:
			res.GatewayClasses = append(res.GatewayClasses, v)
		case *gatewayv1.Gateway:
			res.Gateways = append(res.Gateways, v)
		case *gatewayv1.HTTPRoute:
			res.HTTPRoutes = append(res.HTTPRoutes, v)
		case *corev1.Secret:
			res.Secrets = append(res.Secrets, v)
		case *discoveryv1.EndpointSlice:
			res.EndpointSlices = append(res.EndpointSlices, v)
		case *corev1.Service:
			res.Services = append(res.Services, v)
		case *corev1.Namespace:
			res.Namespaces = append(res.Namespaces, v)
		case *gatewayv1.ReferenceGrant:
			res.ReferenceGrants = append(res.ReferenceGrants, v)
		default:
			t.Fatalf("unsupported test object %T", o)
		}
	}
	return BuildGraph(res)
}

func gatewayOf(t *testing.T, g *Graph, ns, name string) *GatewayInfo {
	t.Helper()
	for _, gw := range g.Gateways {
		if gw.Resource.Namespace == ns && gw.Resource.Name == name {
			return gw
		}
	}
	t.Fatalf("gateway %s/%s not in graph", ns, name)
	return nil
}

func listenerOf(t *testing.T, gw *GatewayInfo, name string) *ListenerInfo {
	t.Helper()
	for _, l := range gw.Listeners {
		if string(l.Spec.Name) == name {
			return l
		}
	}
	t.Fatalf("listener %q not found", name)
	return nil
}

func routeOf(t *testing.T, g *Graph, ns, name string) *RouteInfo {
	t.Helper()
	for _, r := range g.Routes {
		if r.Resource.Namespace == ns && r.Resource.Name == name {
			return r
		}
	}
	t.Fatalf("route %s/%s not in graph", ns, name)
	return nil
}

func parentOf(t *testing.T, r *RouteInfo, gwName string) *RouteParentInfo {
	t.Helper()
	for _, p := range r.Parents {
		if string(p.Ref.Name) == gwName {
			return p
		}
	}
	t.Fatalf("parent %q not found", gwName)
	return nil
}

func condByType(conds []metav1.Condition, ctype string) metav1.Condition {
	for _, c := range conds {
		if c.Type == ctype {
			return c
		}
	}
	return metav1.Condition{}
}
