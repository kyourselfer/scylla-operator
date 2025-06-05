package generateapireference

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	apimachineryutilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

var (
	Scheme = runtime.NewScheme()
	Codecs = serializer.NewCodecFactory(Scheme, serializer.EnableStrict)
)

func init() {
	apimachineryutilruntime.Must(apiextensionsv1.AddToScheme(Scheme))
}
