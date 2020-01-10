module github.com/prometheus/prometheus/cmd/stats

go 1.13

require (
	github.com/imdario/mergo v0.3.8 // indirect
	github.com/openshift/client-go v3.9.0+incompatible
	github.com/pkg/errors v0.8.1
	github.com/prometheus/client_golang v1.3.0
	github.com/prometheus/common v0.7.0
	github.com/prometheus/prometheus v0.0.0-20180315085919-58e2a31db8de
	golang.org/x/oauth2 v0.0.0-20200107190931-bf48bf16ab8d // indirect
	golang.org/x/time v0.0.0-20191024005414-555d28b269f0 // indirect
	gopkg.in/yaml.v2 v2.2.7
	k8s.io/api v0.17.0
	k8s.io/apimachinery v0.17.0
	k8s.io/client-go v0.17.0
	k8s.io/utils v0.0.0-20200108110541-e2fb8e668047 // indirect
)

replace (
	github.com/openshift/client-go => github.com/openshift/client-go v0.0.0-20200107172225-986d9a10f405
	github.com/prometheus/prometheus => ../../
)
