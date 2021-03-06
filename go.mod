module sigs.k8s.io/secrets-store-csi-driver

go 1.13

require (
	github.com/blang/semver v3.5.0+incompatible
	github.com/container-storage-interface/spec v1.0.0
	github.com/golang/protobuf v1.4.2
	github.com/kubernetes-csi/csi-lib-utils v0.6.1
	github.com/kubernetes-csi/csi-test v1.1.0
	github.com/onsi/gomega v1.8.1
	github.com/pkg/errors v0.9.1 // indirect
	github.com/sirupsen/logrus v1.4.2
	github.com/stretchr/testify v1.5.1
	go.opentelemetry.io/otel v0.4.3
	go.opentelemetry.io/otel/exporters/metric/prometheus v0.4.3
	golang.org/x/net v0.0.0-20200222125558-5a598a2470a0
	google.golang.org/grpc v1.27.1
	google.golang.org/protobuf v1.25.0
	k8s.io/api v0.17.2
	k8s.io/apimachinery v0.17.2
	k8s.io/client-go v0.17.2
	k8s.io/utils v0.0.0-20191114184206-e782cd3c129f
	sigs.k8s.io/controller-runtime v0.5.5
)
