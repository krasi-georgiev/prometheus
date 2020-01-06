package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/rulefmt"
	"github.com/prometheus/prometheus/pkg/textparse"
	"github.com/prometheus/prometheus/promql"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	rulesMetrics := getRules()
	fmt.Println("RULES COUNT:", len(rulesMetrics))
	fmt.Println()
	log.Println("reading the scrapes")
	scrapeMetrics := getScrapes("http://localhost:9100/metrics")
	fmt.Println("SCRAPES COUNT:", len(scrapeMetrics))
	fmt.Println()

	var allKeys []string
	var missing []string
	for s := range rulesMetrics {
		if !strings.HasPrefix(s, "node_") {
			continue
		}
		if _, ok := scrapeMetrics[s]; ok {
			allKeys = append(allKeys, s)
			delete(scrapeMetrics, s)
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
	for s := range scrapeMetrics {
		allKeys = append(allKeys, s)
	}
	sort.Strings(allKeys)
	fmt.Println("\n>>>> UNUSED METRICS count:", len(allKeys))
	for _, v := range allKeys {
		fmt.Println(v)
	}
}

func getScrapes(scrapeURLs ...string) map[string]struct{} {
	scrapeMetrics := make(map[string]struct{})
	for _, url := range scrapeURLs {
		data, err := downloadScrape(url)
		if err != nil {
			log.Fatal("get scrape", "url", url, "err", err)
		}

		parser := textparse.New(data, "metrics")
		for {
			et, err := parser.Next()
			if err != nil {
				if err != io.EOF {
					log.Fatal("parse metrics ", err)
				}
				break
			}
			ll := &labels.Labels{}
			if et == textparse.EntrySeries {
				parser.Metric(ll)

				for _, l := range *ll {
					if l.Name == "__name__" {
						scrapeMetrics[l.Value] = struct{}{}
					}
				}
			}
		}
	}
	return scrapeMetrics
}

func getRules() map[string]struct{} {
	config, err := clientcmd.BuildConfigFromFlags("", filepath.Join("..", "..", ".local", "kubeconfig.yml"))
	if err != nil {
		log.Fatal("k8s config file", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal("k8s client set", err)
	}

	configMaps, err := clientset.CoreV1().ConfigMaps("openshift-monitoring").List(metav1.ListOptions{})
	if err != nil {
		log.Println("get configmaps", err)
		log.Println("reading the configmaps cache dir:", cacheDirRules)
		filepath.Walk(cacheDirRules, func(path string, info os.FileInfo, err error) error {
			if info.IsDir() {
				return nil
			}
			f, err := ioutil.ReadFile(path)
			if err != nil {
				log.Fatal("reading rules configmap ", err)
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
	}
	rulesMetrics := make(map[string]struct{})
	for _, m := range configMaps.Items {
		if m.Name == "prometheus-k8s-rulefiles-0" {
			var groups rulefmt.RuleGroups
			for n, content := range m.Data {
				err := ioutil.WriteFile(filepath.Join(cacheDirRules, n), []byte(content), 0644)
				if err != nil {
					log.Fatal("write file configmaps", err)
				}
				if err := yaml.UnmarshalStrict([]byte(content), &groups); err != nil {
					log.Fatal("unmarshal the content", err)
				}

				for _, g := range groups.Groups {
					for _, rule := range g.Rules {
						if rule.Expr != "" {
							_, metrics, err := promql.ParseExpr(rule.Expr)
							if err != nil {
								log.Fatal("parsing the expr ", err)
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
	return rulesMetrics
}

func downloadScrape(url string) ([]byte, error) {
	reg, err := regexp.Compile("[^a-zA-Z0-9]+")
	if err != nil {
		return nil, err
	}
	filename := reg.ReplaceAllString(url, "")
	var data []byte

	resp, err := http.Get(url)
	if err != nil {
		log.Println("get scrape", "url:", url, "err:", err)
		filePath := filepath.Join(cacheDirScrapes, filename)
		log.Println("reading the scrape cache file:", filePath, "url:", url)
		data, err = ioutil.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
	} else {
		err := ioutil.WriteFile(filepath.Join(cacheDirRules, filename), data, 0644)
		if err != nil {
			log.Fatal("write scrape file", "url", url, "err", err)
		}
		data, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		resp.Body.Close()
	}
	return data, err
}
