package server_selector

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pbnjay/memory"
)

// Configuration variables
var (
	apiURL                  string
	bearerToken             string
	testURL                 string
	timeout                 time.Duration
	retries                 int
	retryDelay              time.Duration
	minUptime               float64
	checkInterval           time.Duration
	updateInterval          time.Duration
	lightmodeMaximumServers int
	proxyGroupName          string
	lightMode               bool
	disableUpdateInterval   bool
	maxParallelChecks       int
)

// Proxy represents a proxy server
type Proxy struct {
	Name        string
	Type        string
	DelayMulti  float64
	DelaySingle float64
}

// ProxyResponse represents the response from the API
type ProxyResponse struct {
	Proxies map[string]Proxy `json:"proxies"`
}

func init() {
	apiURL = "http://127.0.0.1:6756"
	bearerToken = "hiddify-server-selector"
	testURL = getEnv("TEST_URL", "http://cp.cloudflare.com")
	timeout = time.Duration(getEnvInt("TIMEOUT", 5000)) * time.Millisecond
	retries = getEnvInt("RETRIES", 12)
	retryDelay = time.Duration(getEnvInt("RETRY_DELAY", 5)) * time.Second
	minUptime = float64(getEnvInt("MIN_UPTIME", 100))
	checkInterval = time.Duration(getEnvInt("CHECK_INTERVAL", 60)) * time.Second
	updateInterval = time.Duration(getEnvInt("UPDATE_INTERVAL", 60)) * time.Minute
	lightmodeMaximumServers = getEnvInt("LIGHTMODE_MAXIMUM_SERVERS", 30)
	proxyGroupName = getEnv("PROXY_GROUP_NAME", "select")
	lightMode = getEnvBool("LIGHT_MODE", true)

	totalMemory := memory.TotalMemory()
	disableUpdateInterval = totalMemory < 512*1024*1024
	if disableUpdateInterval {
		fmt.Println("Low memory detected. Update interval disabled.")
	}

	maxParallelChecks = 30           // Default value
	if totalMemory < 512*1024*1024 { // 512MB
		maxParallelChecks = 10
	}
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value, exists := os.LookupEnv(key); exists {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value, exists := os.LookupEnv(key); exists {
		return strings.ToLower(value) == "true" || value == "1"
	}
	return defaultValue
}

func getHeaders() http.Header {
	headers := http.Header{}
	headers.Set("Authorization", fmt.Sprintf("Bearer %s", bearerToken))
	return headers
}

func getProxies() (map[string]Proxy, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/proxies", apiURL), nil)
	if err != nil {
		return nil, err
	}
	req.Header = getHeaders()

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get proxies: %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var proxyResp ProxyResponse
	if err := json.Unmarshal(body, &proxyResp); err != nil {
		return nil, err
	}

	return proxyResp.Proxies, nil
}

func getRealDelayMulti(proxyName string) float64 {
	delays := make([]float64, 0, retries)
	client := &http.Client{Timeout: timeout}

	for i := 0; i < retries; i++ {
		req, err := http.NewRequest("GET", fmt.Sprintf("%s/proxies/%s/delay?timeout=%d&url=%s", apiURL, proxyName, int(timeout.Milliseconds()), testURL), nil)
		if err != nil {
			fmt.Printf("Error creating request for %s: %v\n", proxyName, err)
			delays = append(delays, float64(timeout.Milliseconds()))
			continue
		}
		req.Header = getHeaders()

		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			// fmt.Printf("Error getting delay for %s: %v\n", proxyName, err)
			delays = append(delays, float64(timeout.Milliseconds()))
		} else {
			elapsed := time.Since(start)
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var result struct {
					Delay int `json:"delay"`
				}
				body, _ := ioutil.ReadAll(resp.Body)
				if err := json.Unmarshal(body, &result); err == nil {
					delays = append(delays, float64(result.Delay))
				} else {
					delays = append(delays, float64(elapsed.Milliseconds()))
				}
			} else {
				delays = append(delays, float64(timeout.Milliseconds()))
			}
		}

		timeoutCount := 0
		for _, d := range delays {
			if d >= float64(timeout.Milliseconds()) {
				timeoutCount++
			}
		}
		if float64(timeoutCount) > float64(retries)*(1-minUptime/100) {
			break
		}

		if i < retries-1 {
			time.Sleep(retryDelay)
		}
	}

	if len(delays) > 0 {
		fmt.Println(delays)
		sum := 0.0
		count := 0
		for _, delay := range delays {
			if delay < float64(timeout.Milliseconds()) {
				sum += delay
				count++
			}
		}
		if float64(count) >= float64(retries)*(minUptime/100) {
			return sum / float64(count)
		}
	}

	return math.Inf(1)
}

func getRealDelaySingle(proxyName string) float64 {
	client := &http.Client{Timeout: timeout}
	numberOfAttempts := 1

	for attempt := 0; attempt < numberOfAttempts; attempt++ {
		req, err := http.NewRequest("GET", fmt.Sprintf("%s/proxies/%s/delay?timeout=%d&url=%s", apiURL, proxyName, int(timeout.Milliseconds()), testURL), nil)
		if err != nil {
			continue
		}
		req.Header = getHeaders()

		resp, err := client.Do(req)
		if err != nil {
			if attempt < numberOfAttempts-1 {
				fmt.Printf("Timeout error for %s, retrying in 10 seconds...\n", proxyName)
				time.Sleep(10 * time.Second)
			}
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var result struct {
				Delay int `json:"delay"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err == nil {
				return float64(result.Delay)
			}
		}
	}

	return float64(timeout.Milliseconds())
}

func updateDelayInfo(proxies map[string]Proxy, samplingType string, stop <-chan struct{}) {
	var wg sync.WaitGroup
	var mu sync.Mutex // Mutex to protect the map

	sem := make(chan struct{}, maxParallelChecks)

	for name, proxy := range proxies {
		select {
		case <-stop:
			return // Exit immediately if stop signal is received
		default:
			if proxy.Type == "VLESS" || proxy.Type == "Trojan" || proxy.Type == "Shadowsocks" || proxy.Type == "VMess" || proxy.Type == "TUIC" || proxy.Type == "WireGuard" {
				wg.Add(1)
				go func(name string, proxy Proxy) {
					sem <- struct{}{} // Acquire semaphore
					defer func() {
						<-sem // Release semaphore
						wg.Done()
					}()

					if samplingType == "multi" {
						delay := getRealDelayMulti(name)
						mu.Lock() // Lock before updating the map
						proxy.DelayMulti = delay
						proxies[name] = proxy
						mu.Unlock() // Unlock after updating
						fmt.Printf("Updated multi delay for %s: %.2f ms\n", name, delay)
					} else if samplingType == "single" {
						delay := getRealDelaySingle(name)
						mu.Lock() // Lock before updating the map
						proxy.DelaySingle = delay
						proxies[name] = proxy
						mu.Unlock() // Unlock after updating
						fmt.Printf("Updated single delay for %s: %.2f ms\n", name, delay)
					}
				}(name, proxy)
			}
		}
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-stop:
		return // Exit if stop signal is received
	case <-done:
		return // Exit when all goroutines are done
	}
}

// Todo Function: Sort Proxies by least number of Timeouts

func sortProxiesByDelay(proxies map[string]Proxy, samplingType string) []Proxy {
	var sortableProxies []Proxy
	for _, proxy := range proxies {
		if proxy.Type == "VLESS" || proxy.Type == "Trojan" || proxy.Type == "Shadowsocks" || proxy.Type == "VMess" || proxy.Type == "TUIC" || proxy.Type == "WireGuard" {
			sortableProxies = append(sortableProxies, proxy)
		}
	}

	if samplingType == "multi" {
		// Filter out proxies with DelayMulti equal to math.Inf(1)
		var filteredProxies []Proxy
		for _, proxy := range sortableProxies {
			if proxy.DelayMulti != math.Inf(1) {
				filteredProxies = append(filteredProxies, proxy)
			}
		}
		sortableProxies = filteredProxies

		// Sort the filtered proxies
		sort.Slice(sortableProxies, func(i, j int) bool {
			return sortableProxies[i].DelayMulti < sortableProxies[j].DelayMulti
		})
	} else {
		sort.Slice(sortableProxies, func(i, j int) bool {
			return sortableProxies[i].DelaySingle < sortableProxies[j].DelaySingle
		})
	}

	return sortableProxies
}

func filterSingleWorkingProxies(proxies map[string]Proxy) []Proxy {
	var workingProxies []Proxy
	for _, proxy := range proxies {
		if (proxy.Type == "VLESS" || proxy.Type == "Trojan" || proxy.Type == "Shadowsocks" || proxy.Type == "VMess" || proxy.Type == "TUIC" || proxy.Type == "WireGuard") &&
			proxy.DelaySingle < float64(timeout.Milliseconds()) {
			workingProxies = append(workingProxies, proxy)
		}
	}

	sort.Slice(workingProxies, func(i, j int) bool {
		return workingProxies[i].DelaySingle < workingProxies[j].DelaySingle
	})

	return workingProxies
}

func fallbackToWorkingProxyByOrder(sortedProxies []Proxy) error {
	client := &http.Client{Timeout: timeout}

	for _, proxy := range sortedProxies {
		req, err := http.NewRequest("GET", fmt.Sprintf("%s/proxies/%s/delay?timeout=%d&url=%s", apiURL, proxy.Name, int(timeout.Milliseconds()), testURL), nil)
		if err != nil {
			continue
		}
		req.Header = getHeaders()

		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("%s timed out. Trying the next one...\n", proxy.Name)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			fmt.Printf("Switching to %s\n", proxy.Name)
			switchReq, err := http.NewRequest("PUT", fmt.Sprintf("%s/proxies/%s", apiURL, proxyGroupName), strings.NewReader(fmt.Sprintf(`{"name":"%s"}`, proxy.Name)))
			if err != nil {
				return err
			}
			switchReq.Header = getHeaders()
			switchReq.Header.Set("Content-Type", "application/json")

			switchResp, err := client.Do(switchReq)
			if err != nil {
				return err
			}
			switchResp.Body.Close()
			return nil
		} else if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%s is not found", proxy.Name)
		} else {
			fmt.Printf("%s is not responding. Trying the next one...\n", proxy.Name)
		}
	}

	fmt.Println("No working proxies found.")
	return nil
}

func MainLoop(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			fmt.Println("Stopping main loop...")
			return
		default:
			proxies, err := getProxies()
			if err != nil {
				fmt.Printf("An error occurred: %v\n", err)
				if !interruptibleSleep(5*time.Minute, stop) {
					return
				}
				fmt.Println("Restarting The Loop...")
				continue
			}

			fmt.Printf("Fetched %d Proxies!\n", len(proxies))

			var sortedProxies []Proxy
			if lightMode {
				updateDelayInfo(proxies, "single", stop)
				workingProxies := filterSingleWorkingProxies(proxies)
				if len(workingProxies) > lightmodeMaximumServers {
					workingProxies = workingProxies[:lightmodeMaximumServers]
				}
				workingProxiesMap := make(map[string]Proxy)
				for _, proxy := range workingProxies {
					workingProxiesMap[proxy.Name] = proxy
				}
				updateDelayInfo(workingProxiesMap, "multi", stop)
				sortedProxies = sortProxiesByDelay(workingProxiesMap, "multi")
			} else {
				updateDelayInfo(proxies, "multi", stop)
				sortedProxies = sortProxiesByDelay(proxies, "multi")
			}

			if disableUpdateInterval {
				// If updateInterval is disabled, just perform one check and sleep
				if err := fallbackToWorkingProxyByOrder(sortedProxies); err != nil {
					fmt.Printf("Error during fallback: %v\n", err)
				}
				if !interruptibleSleep(checkInterval, stop) {
					return
				}
			} else {
				// Original behavior with updateInterval
				startTime := time.Now()
				for time.Since(startTime) < updateInterval {
					select {
					case <-stop:
						fmt.Println("Stopping main loop...")
						return
					default:
						if err := fallbackToWorkingProxyByOrder(sortedProxies); err != nil {
							fmt.Printf("Error during fallback: %v\n", err)
						}
						if !interruptibleSleep(checkInterval, stop) {
							return
						}
					}
				}
			}
		}
	}
}

func interruptibleSleep(d time.Duration, stop <-chan struct{}) bool {
	timer := time.NewTimer(d)
	select {
	case <-timer.C:
		return true
	case <-stop:
		if !timer.Stop() {
			<-timer.C
		}
		fmt.Println("Sleep interrupted, stopping...")
		return false
	}
}

func main() {
	stop := make(chan struct{})
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt, syscall.SIGTERM)
		<-sigint
		fmt.Println("Received interrupt signal. Shutting down...")
		close(stop)
	}()

	MainLoop(stop)
	fmt.Println("Program exited gracefully")
}
