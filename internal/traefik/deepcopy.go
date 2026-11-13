package traefik

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DeepCopy returns a deep copy of the Middleware.
func (in *Middleware) DeepCopy() *Middleware {
	if in == nil {
		return nil
	}
	out := new(Middleware)
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = *in.Spec.DeepCopy()
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *Middleware) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

// DeepCopy returns a deep copy of the MiddlewareSpec.
func (in *MiddlewareSpec) DeepCopy() *MiddlewareSpec {
	if in == nil {
		return nil
	}
	out := new(MiddlewareSpec)
	if in.IPAllowList != nil {
		out.IPAllowList = in.IPAllowList.DeepCopy()
	}
	if in.RedirectRegex != nil {
		out.RedirectRegex = in.RedirectRegex.DeepCopy()
	}
	if in.RedirectScheme != nil {
		out.RedirectScheme = in.RedirectScheme.DeepCopy()
	}
	if in.Headers != nil {
		out.Headers = in.Headers.DeepCopy()
	}
	if in.ReplacePath != nil {
		out.ReplacePath = in.ReplacePath.DeepCopy()
	}
	if in.ReplacePathRegex != nil {
		out.ReplacePathRegex = in.ReplacePathRegex.DeepCopy()
	}
	if in.StripPrefix != nil {
		out.StripPrefix = in.StripPrefix.DeepCopy()
	}
	if in.StripPrefixRegex != nil {
		out.StripPrefixRegex = in.StripPrefixRegex.DeepCopy()
	}
	if in.AddPrefix != nil {
		out.AddPrefix = in.AddPrefix.DeepCopy()
	}
	return out
}

// DeepCopy returns a deep copy of the IPAllowList.
func (in *IPAllowList) DeepCopy() *IPAllowList {
	if in == nil {
		return nil
	}
	out := new(IPAllowList)
	*out = *in
	if in.SourceRange != nil {
		out.SourceRange = append([]string(nil), in.SourceRange...)
	}
	if in.IPStrategy != nil {
		out.IPStrategy = in.IPStrategy.DeepCopy()
	}
	return out
}

// DeepCopy returns a deep copy of the IPStrategy.
func (in *IPStrategy) DeepCopy() *IPStrategy {
	if in == nil {
		return nil
	}
	out := new(IPStrategy)
	*out = *in
	if in.ExcludedIPs != nil {
		out.ExcludedIPs = append([]string(nil), in.ExcludedIPs...)
	}
	return out
}

// DeepCopy returns a deep copy of the RedirectRegex.
func (in *RedirectRegex) DeepCopy() *RedirectRegex {
	if in == nil {
		return nil
	}
	out := new(RedirectRegex)
	*out = *in
	return out
}

// DeepCopy returns a deep copy of the RedirectScheme.
func (in *RedirectScheme) DeepCopy() *RedirectScheme {
	if in == nil {
		return nil
	}
	out := new(RedirectScheme)
	*out = *in
	return out
}

// DeepCopy returns a deep copy of the ReplacePath.
func (in *ReplacePath) DeepCopy() *ReplacePath {
	if in == nil {
		return nil
	}
	out := new(ReplacePath)
	*out = *in
	return out
}

// DeepCopy returns a deep copy of the ReplacePathRegex.
func (in *ReplacePathRegex) DeepCopy() *ReplacePathRegex {
	if in == nil {
		return nil
	}
	out := new(ReplacePathRegex)
	*out = *in
	return out
}

// DeepCopy returns a deep copy of the StripPrefix.
func (in *StripPrefix) DeepCopy() *StripPrefix {
	if in == nil {
		return nil
	}
	out := new(StripPrefix)
	*out = *in
	if in.Prefixes != nil {
		out.Prefixes = append([]string(nil), in.Prefixes...)
	}
	return out
}

// DeepCopy returns a deep copy of the StripPrefixRegex.
func (in *StripPrefixRegex) DeepCopy() *StripPrefixRegex {
	if in == nil {
		return nil
	}
	out := new(StripPrefixRegex)
	*out = *in
	if in.Regex != nil {
		out.Regex = append([]string(nil), in.Regex...)
	}
	return out
}

// DeepCopy returns a deep copy of the AddPrefix.
func (in *AddPrefix) DeepCopy() *AddPrefix {
	if in == nil {
		return nil
	}
	out := new(AddPrefix)
	*out = *in
	return out
}

// DeepCopy returns a deep copy of the Headers middleware.
func (in *Headers) DeepCopy() *Headers {
	if in == nil {
		return nil
	}
	out := new(Headers)
	*out = *in
	out.CustomRequestHeaders = copyStringMap(in.CustomRequestHeaders)
	out.CustomResponseHeaders = copyStringMap(in.CustomResponseHeaders)
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// DeepCopy returns a deep copy of the MiddlewareList.
func (in *MiddlewareList) DeepCopy() *MiddlewareList {
	if in == nil {
		return nil
	}
	out := new(MiddlewareList)
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Middleware, len(in.Items))
		for i := range in.Items {
			out.Items[i] = *in.Items[i].DeepCopy()
		}
	}
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *MiddlewareList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

// DeepCopy returns a deep copy of the IngressRouteTCP.
func (in *IngressRouteTCP) DeepCopy() *IngressRouteTCP {
	if in == nil {
		return nil
	}
	out := new(IngressRouteTCP)
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = *in.Spec.DeepCopy()
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *IngressRouteTCP) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

// DeepCopy returns a deep copy of the IngressRouteTCPSpec.
func (in *IngressRouteTCPSpec) DeepCopy() *IngressRouteTCPSpec {
	if in == nil {
		return nil
	}
	out := new(IngressRouteTCPSpec)
	if in.EntryPoints != nil {
		out.EntryPoints = append([]string(nil), in.EntryPoints...)
	}
	if in.Routes != nil {
		out.Routes = make([]RouteTCP, len(in.Routes))
		for i := range in.Routes {
			in.Routes[i].DeepCopyInto(&out.Routes[i])
		}
	}
	if in.TLS != nil {
		out.TLS = in.TLS.DeepCopy()
	}
	return out
}

// DeepCopyInto copies the RouteTCP into out.
func (in *RouteTCP) DeepCopyInto(out *RouteTCP) {
	*out = *in
	if in.Services != nil {
		out.Services = make([]ServiceTCP, len(in.Services))
		for i := range in.Services {
			in.Services[i].DeepCopyInto(&out.Services[i])
		}
	}
}

// DeepCopy returns a deep copy of the RouteTCP.
func (in *RouteTCP) DeepCopy() *RouteTCP {
	if in == nil {
		return nil
	}
	out := new(RouteTCP)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the ServiceTCP into out.
func (in *ServiceTCP) DeepCopyInto(out *ServiceTCP) {
	*out = *in
	if in.ProxyProtocol != nil {
		out.ProxyProtocol = in.ProxyProtocol.DeepCopy()
	}
}

// DeepCopy returns a deep copy of the ServiceTCP.
func (in *ServiceTCP) DeepCopy() *ServiceTCP {
	if in == nil {
		return nil
	}
	out := new(ServiceTCP)
	in.DeepCopyInto(out)
	return out
}

// DeepCopy returns a deep copy of the ProxyProtocol.
func (in *ProxyProtocol) DeepCopy() *ProxyProtocol {
	if in == nil {
		return nil
	}
	out := new(ProxyProtocol)
	*out = *in
	return out
}

// DeepCopy returns a deep copy of the TLS block.
func (in *TLS) DeepCopy() *TLS {
	if in == nil {
		return nil
	}
	out := new(TLS)
	*out = *in
	return out
}

// DeepCopy returns a deep copy of the IngressRouteTCPList.
func (in *IngressRouteTCPList) DeepCopy() *IngressRouteTCPList {
	if in == nil {
		return nil
	}
	out := new(IngressRouteTCPList)
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]IngressRouteTCP, len(in.Items))
		for i := range in.Items {
			out.Items[i] = *in.Items[i].DeepCopy()
		}
	}
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *IngressRouteTCPList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

var (
	_ runtime.Object = &Middleware{}
	_ runtime.Object = &MiddlewareList{}
	_ runtime.Object = &IngressRouteTCP{}
	_ runtime.Object = &IngressRouteTCPList{}
	_ metav1.Object  = &Middleware{}
	_ metav1.Object  = &IngressRouteTCP{}
)
