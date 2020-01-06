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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/pkg/rulefmt"
	"github.com/prometheus/prometheus/promql"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/prometheus/client_golang/api"
	promV1 "github.com/prometheus/client_golang/api/prometheus/v1"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	cacheDirRules   = filepath.Join("..", "..", ".local", "cache", "rules")
	cacheDirScrapes = filepath.Join("..", "..", ".local", "cache", "scrapes")
)

func main() {
	if err := os.MkdirAll(cacheDirRules, os.ModePerm); err != nil {
		log.Fatal("create rules cache dir:", err)
	}
	if err := os.MkdirAll(cacheDirScrapes, os.ModePerm); err != nil {
		log.Fatal("create scrapes cache dir:", err)
	}

	log.Println("reading the rules")
	rulesMetrics, err := getRules()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("RULES COUNT:", len(rulesMetrics))
	fmt.Println()
	log.Println("reading the scrapes")
	scrapedMetrics, err := getScrapedMetrics("https://prometheus-k8s-openshift-monitoring.apps.ci-ln-xrqlsl2-d5d6b.origin-ci-int-aws.dev.rhcloud.com/")
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

func getScrapedMetrics(url string) (map[string]struct{}, error) {
	scrapedMetrics := make(map[string]struct{})
	reg, err := regexp.Compile("[^a-zA-Z0-9]+")
	if err != nil {
		return nil, errors.Wrap(err, "regex for filename")
	}

	filename := reg.ReplaceAllString(url, "") + ".json"
	var data []byte

	client, err := api.NewClient(api.Config{
		Address: url,
		RoundTripper: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	})
	if err != nil {
		return nil, errors.Wrap(err, "create prom api client")
	}

	v1api := promV1.NewAPI(client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	labels, warn, err := v1api.LabelValues(ctx, "__name__")
	if warn != nil {
		log.Println("api call to get the prom labels returned warnings:", warn)
	}
	if err != nil {
		log.Println("get scrape", "url:", url, "err:", err)
		filePath := filepath.Join(cacheDirScrapes, filename)
		log.Println("reading the scrape cache file:", filePath, "url:", url)
		data, err = ioutil.ReadFile(filePath)
		if err != nil {
			return nil, errors.Wrap(err, "reading scrape metrics cache file")
		}
		err := json.Unmarshal(data, &labels)
		if err != nil {
			return nil, errors.Wrap(err, "unmarshal scrape cache file")
		}
	} else {
		data, err := json.Marshal(labels)
		if err != nil {
			return nil, errors.Wrap(err, "marshal scrape labels")
		}
		err = ioutil.WriteFile(filepath.Join(cacheDirRules, filename), data, 0644)
		if err != nil {
			return nil, errors.Wrapf(err, "write scrape file url:%v", url)
		}

	}

	for _, l := range labels {
		scrapedMetrics[string(l)] = struct{}{}
	}

	return scrapedMetrics, nil
}

func getRules() (map[string]struct{}, error) {
	config, err := clientcmd.BuildConfigFromFlags("", filepath.Join("..", "..", ".local", "kubeconfig.yml"))
	if err != nil {
		return nil, errors.Wrap(err, "k8s config file")
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "k8s client set")
	}

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
