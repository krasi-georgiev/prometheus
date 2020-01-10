package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	routev1 "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/rulefmt"
	"github.com/prometheus/prometheus/promql"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/client-go/transport"

	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/prometheus/client_golang/api"
	promV1 "github.com/prometheus/client_golang/api/prometheus/v1"

	"k8s.io/client-go/tools/clientcmd"
)

var (
	cacheDirRules           = filepath.Join("..", "..", ".local", "cache", "rules")
	cacheDirScrapes         = filepath.Join("..", "..", ".local", "cache", "scrapes")
	cacheDirScrapesFilename = filepath.Join(cacheDirScrapes, "scrapes.json")
)

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)

	if err := os.MkdirAll(cacheDirRules, os.ModePerm); err != nil {
		log.Fatal("create rules cache dir:", err)
	}
	if err := os.MkdirAll(cacheDirScrapes, os.ModePerm); err != nil {
		log.Fatal("create scrapes cache dir:", err)
	}

	config, err := clientcmd.BuildConfigFromFlags("", filepath.Join(os.Getenv("HOME"), ".kube", "config"))
	if err != nil {
		log.Fatal("k8s config file", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal("k8s client set:", err)
	}
	log.Println("reading the rules")
	rulesMetrics, err := getRules(clientset)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("RULES COUNT:", len(rulesMetrics))
	fmt.Println()
	log.Println("reading the scrapes")

	routeClient, err := routev1.NewForConfig(config)
	if err != nil {
		log.Fatal("creating openshiftClient failed:", err)
	}

	scrapedMetrics, err := getScrapedMetrics(clientset, routeClient)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("SCRAPES COUNT:", len(scrapedMetrics))
	fmt.Println()

	var allKeys []string
	var missing []string
	for s := range rulesMetrics {
		if _, ok := scrapedMetrics[s]; ok {
			allKeys = append(allKeys, s)
			delete(scrapedMetrics, s)
			continue
		}
		missing = append(missing, s)
	}

	sort.Strings(missing)
	fmt.Println("\n>>>> MISSING METRICS count:", len(missing))
	for _, v := range missing {
		fmt.Println(v)
	}

	sort.Strings(allKeys)
	fmt.Println("\n>>>> USED METRICS count:", len(allKeys))
	for _, v := range allKeys {
		fmt.Println(v)
	}

	allKeys = allKeys[:0]
	for s := range scrapedMetrics {
		allKeys = append(allKeys, s)
	}
	sort.Strings(allKeys)
	fmt.Println("\n>>>> UNUSED METRICS count:", len(allKeys))
	for _, v := range allKeys {
		fmt.Println(v)
	}
}

func promURL(host string) (string, error) {
	u, err := url.Parse(host)
	if err != nil {
		return "", err
	}
	host, _, _ = net.SplitHostPort(u.Host)
	i := strings.Index(host, ".")
	host = host[i+1:]
	return "https://prometheus-k8s-openshift-monitoring.apps." + host, nil
}

func getScrapedMetrics(kubeClient kubernetes.Interface, routeClient routev1.RouteV1Interface) (scrapedMetrics map[string]struct{}, err error) {
	var (
		labels model.LabelValues
	)
	defer func() {
		if err != nil {
			err = nil
			log.Println("reading the scrape cache file:", cacheDirScrapesFilename)
			data, err := ioutil.ReadFile(cacheDirScrapesFilename)
			if err != nil {
				err = errors.Wrap(err, "reading scrape metrics cache file")
				return
			}

			if err := json.Unmarshal(data, &labels); err != nil {
				err = errors.Wrap(err, "unmarshal scrape cache file")
				return
			}
		}
		scrapedMetrics = make(map[string]struct{})
		for _, l := range labels {
			scrapedMetrics[string(l)] = struct{}{}
		}
	}()
	cl1, err := createServiceAccount(kubeClient)
	if err != nil {
		log.Println("creating the service account err:", err)
		return
	}
	defer cl1()

	cl2, err := createClusterRoleBinding(kubeClient)
	if err != nil {
		log.Println("creating the cluster role binding err:", err)
		return
	}
	defer cl2()

	url, err := getPromURL(routeClient)
	if err != nil {
		log.Println("getting the Prom url err:", err)
		return
	}
	secret, err := getSecret(kubeClient)
	if err != nil {
		log.Println("getting the Prom secret err:", err)
		return
	}

	client, err := api.NewClient(api.Config{
		Address: url,
		RoundTripper: transport.NewBearerAuthRoundTripper(secret, &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}),
	})
	if err != nil {
		log.Println("creating Prom client err:", err)
		return
	}

	v1api := promV1.NewAPI(client)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	labels, warn, err := v1api.LabelValues(ctx, "__name__")
	if warn != nil {
		log.Println("api call to get the prom labels returned warnings:", warn)
	}
	if err != nil {
		log.Println("get the metric names err:", err)
		return
	}

	data, err := json.Marshal(labels)
	if err != nil {
		log.Println("marshal metric labels err:", err)
		return
	}

	if err := ioutil.WriteFile(cacheDirScrapesFilename, data, 0644); err != nil {
		log.Println(err, "write scrape cache file err:", url)
	}
	return
}

func getRules(clientset kubernetes.Interface) (map[string]struct{}, error) {

	configMaps, err := clientset.CoreV1().ConfigMaps("openshift-monitoring").List(metav1.ListOptions{})
	if err != nil {
		log.Println("get configmaps", err)
		log.Println("reading the configmaps cache dir:", cacheDirRules)
		err := filepath.Walk(cacheDirRules, func(path string, info os.FileInfo, err error) error {
			if info.IsDir() {
				return nil
			}
			f, err := ioutil.ReadFile(path)
			if err != nil {
				return err
			}
			data := map[string]string{
				info.Name(): string(f),
			}
			configMaps.Items = append(configMaps.Items, v1.ConfigMap{
				Data:       data,
				ObjectMeta: metav1.ObjectMeta{Name: "prometheus-k8s-rulefiles-0"},
			})
			return nil
		})
		if err != nil {
			return nil, errors.Wrap(err, "reading rules configmap")
		}
	}
	rulesMetrics := make(map[string]struct{})
	for _, m := range configMaps.Items {
		if m.Name == "prometheus-k8s-rulefiles-0" {
			var groups rulefmt.RuleGroups
			for n, content := range m.Data {
				err := ioutil.WriteFile(filepath.Join(cacheDirRules, n), []byte(content), 0644)
				if err != nil {
					return nil, errors.Wrap(err, "write file configmaps")
				}
				if err := yaml.UnmarshalStrict([]byte(content), &groups); err != nil {
					return nil, errors.Wrap(err, "unmarshal the content")
				}

				for _, g := range groups.Groups {
					for _, rule := range g.Rules {
						if rule.Expr != "" {
							_, metrics, err := promql.ParseExpr(rule.Expr)
							if err != nil {
								return nil, errors.Wrap(err, "parsing the expr")
							}
							for m := range metrics {
								rulesMetrics[m] = struct{}{}
							}
						}
					}
				}
			}
		}
	}
	return rulesMetrics, nil
}

func createServiceAccount(kubeClient kubernetes.Interface) (cleanUpFunc, error) {
	serviceAccount := &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-monitoring-operator-e2e",
			Namespace: "openshift-monitoring",
		},
	}

	serviceAccount, err := kubeClient.CoreV1().ServiceAccounts("openshift-monitoring").Create(serviceAccount)
	if err != nil {
		return nil, err
	}

	return func() error {
		return kubeClient.CoreV1().ServiceAccounts("openshift-monitoring").Delete(serviceAccount.Name, &metav1.DeleteOptions{})
	}, nil
}

func createClusterRoleBinding(kubeClient kubernetes.Interface) (cleanUpFunc, error) {
	clusterRoleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-monitoring-operator-e2e",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "cluster-monitoring-operator-e2e",
				Namespace: "openshift-monitoring",
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     "cluster-monitoring-view",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}

	clusterRoleBinding, err := kubeClient.RbacV1().ClusterRoleBindings().Create(clusterRoleBinding)
	if err != nil {
		return nil, err
	}

	return func() error {
		return kubeClient.RbacV1().ClusterRoleBindings().Delete(clusterRoleBinding.Name, &metav1.DeleteOptions{})
	}, nil
}

func getPromURL(routeClient routev1.RouteV1Interface) (string, error) {
	route, err := routeClient.Routes("openshift-monitoring").Get("thanos-querier", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	host := "https://" + route.Spec.Host
	return host, nil
}

func getSecret(kubeClient kubernetes.Interface) (string, error) {
	secrets, err := kubeClient.CoreV1().Secrets("openshift-monitoring").List(metav1.ListOptions{})
	if err != nil {
		return "", err
	}

	var token string
	for _, secret := range secrets.Items {
		_, dockerToken := secret.Annotations["openshift.io/create-dockercfg-secrets"]
		e2eToken := strings.Contains(secret.Name, "cluster-monitoring-operator-e2e-token-")

		// we have to skip the token secret that contains the openshift.io/create-dockercfg-secrets annotation
		// as this is the token to talk to the internal registry.
		if !dockerToken && e2eToken {
			token = string(secret.Data["token"])
		}
	}
	return token, nil
}

type cleanUpFunc func() error
