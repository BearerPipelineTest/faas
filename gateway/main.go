// Copyright (c) Alex Ellis 2017. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/openfaas/faas-provider/auth"
	"github.com/openfaas/faas/gateway/handlers"
	"github.com/openfaas/faas/gateway/metrics"
	"github.com/openfaas/faas/gateway/pkg/middleware"
	"github.com/openfaas/faas/gateway/plugin"
	"github.com/openfaas/faas/gateway/probing"
	"github.com/openfaas/faas/gateway/scaling"
	"github.com/openfaas/faas/gateway/types"
	"github.com/openfaas/faas/gateway/version"
	natsHandler "github.com/openfaas/nats-queue-worker/handler"
)

// NameExpression for a function / service
const NameExpression = "-a-zA-Z_0-9."

func main() {

	osEnv := types.OsEnv{}
	readConfig := types.ReadConfig{}
	config, configErr := readConfig.Read(osEnv)

	if configErr != nil {
		log.Fatalln(configErr)
	}
	if !config.UseExternalProvider() {
		log.Fatalln("You must provide an external provider via 'functions_provider_url' env-var.")
	}

	fmt.Printf("OpenFaaS Gateway - Community Edition (CE)\n"+
		"\nVersion: %s Commit: %s\nTimeouts: read=%s\twrite=%s\tupstream=%s\nFunction provider: %s\n\n",
		version.BuildVersion(),
		version.GitCommitSHA,
		config.ReadTimeout,
		config.WriteTimeout,
		config.UpstreamTimeout,
		config.FunctionsProviderURL)

	// credentials is used for service-to-service auth
	var credentials *auth.BasicAuthCredentials

	if config.UseBasicAuth {
		var readErr error
		reader := auth.ReadBasicAuthFromDisk{
			SecretMountPath: config.SecretMountPath,
		}
		credentials, readErr = reader.Read()

		if readErr != nil {
			log.Panicf(readErr.Error())
		}
	}

	var faasHandlers types.HandlerSet

	servicePollInterval := time.Second * 5

	metricsOptions := metrics.BuildMetricsOptions()
	exporter := metrics.NewExporter(metricsOptions, credentials, config.Namespace)
	exporter.StartServiceWatcher(*config.FunctionsProviderURL, metricsOptions, "func", servicePollInterval)
	metrics.RegisterExporter(exporter)

	reverseProxy := types.NewHTTPClientReverseProxy(config.FunctionsProviderURL,
		config.UpstreamTimeout,
		config.MaxIdleConns,
		config.MaxIdleConnsPerHost)

	loggingNotifier := handlers.LoggingNotifier{}

	prometheusNotifier := handlers.PrometheusFunctionNotifier{
		Metrics:           &metricsOptions,
		FunctionNamespace: config.Namespace,
	}

	prometheusServiceNotifier := handlers.PrometheusServiceNotifier{
		ServiceMetrics: metricsOptions.ServiceMetrics,
	}

	functionNotifiers := []handlers.HTTPNotifier{loggingNotifier, prometheusNotifier}
	forwardingNotifiers := []handlers.HTTPNotifier{loggingNotifier, prometheusServiceNotifier}
	quietNotifier := []handlers.HTTPNotifier{prometheusServiceNotifier}

	urlResolver := middleware.SingleHostBaseURLResolver{BaseURL: config.FunctionsProviderURL.String()}
	var functionURLResolver middleware.BaseURLResolver
	var functionURLTransformer middleware.URLPathTransformer
	nilURLTransformer := middleware.TransparentURLPathTransformer{}
	trimURLTransformer := middleware.FunctionPrefixTrimmingURLPathTransformer{}

	if config.DirectFunctions {
		functionURLResolver = middleware.FunctionAsHostBaseURLResolver{
			FunctionSuffix:    config.DirectFunctionsSuffix,
			FunctionNamespace: config.Namespace,
		}
		functionURLTransformer = trimURLTransformer
	} else {
		functionURLResolver = urlResolver
		functionURLTransformer = nilURLTransformer
	}

	var serviceAuthInjector middleware.AuthInjector

	if config.UseBasicAuth {
		serviceAuthInjector = &middleware.BasicAuthInjector{Credentials: credentials}
	}

	decorateExternalAuth := handlers.MakeExternalAuthHandler

	// externalServiceQuery is used to query metadata from the provider about a function
	externalServiceQuery := plugin.NewExternalServiceQuery(*config.FunctionsProviderURL, serviceAuthInjector)

	scalingConfig := scaling.ScalingConfig{
		MaxPollCount:         uint(1000),
		SetScaleRetries:      uint(20),
		FunctionPollInterval: time.Millisecond * 100,
		CacheExpiry:          time.Millisecond * 250, // freshness of replica values before going stale
		ServiceQuery:         externalServiceQuery,
	}

	// This cache can be used to query a function's annotations.
	functionAnnotationCache := scaling.NewFunctionCache(scalingConfig.CacheExpiry)
	cachedFunctionQuery := scaling.NewCachedFunctionQuery(functionAnnotationCache, externalServiceQuery)

	faasHandlers.Proxy = handlers.MakeCallIDMiddleware(
		handlers.MakeForwardingProxyHandler(reverseProxy, functionNotifiers, functionURLResolver, functionURLTransformer, nil),
	)

	faasHandlers.ListFunctions = handlers.MakeForwardingProxyHandler(reverseProxy, forwardingNotifiers, urlResolver, nilURLTransformer, serviceAuthInjector)
	faasHandlers.DeployFunction = handlers.MakeForwardingProxyHandler(reverseProxy, forwardingNotifiers, urlResolver, nilURLTransformer, serviceAuthInjector)
	faasHandlers.DeleteFunction = handlers.MakeForwardingProxyHandler(reverseProxy, forwardingNotifiers, urlResolver, nilURLTransformer, serviceAuthInjector)
	faasHandlers.UpdateFunction = handlers.MakeForwardingProxyHandler(reverseProxy, forwardingNotifiers, urlResolver, nilURLTransformer, serviceAuthInjector)
	faasHandlers.FunctionStatus = handlers.MakeForwardingProxyHandler(reverseProxy, forwardingNotifiers, urlResolver, nilURLTransformer, serviceAuthInjector)

	faasHandlers.InfoHandler = handlers.MakeInfoHandler(handlers.MakeForwardingProxyHandler(reverseProxy, forwardingNotifiers, urlResolver, nilURLTransformer, serviceAuthInjector))
	faasHandlers.SecretHandler = handlers.MakeForwardingProxyHandler(reverseProxy, forwardingNotifiers, urlResolver, nilURLTransformer, serviceAuthInjector)

	faasHandlers.NamespaceListerHandler = handlers.MakeForwardingProxyHandler(reverseProxy, forwardingNotifiers, urlResolver, nilURLTransformer, serviceAuthInjector)

	faasHandlers.Alert = handlers.MakeNotifierWrapper(
		handlers.MakeAlertHandler(externalServiceQuery, config.Namespace),
		quietNotifier,
	)

	faasHandlers.LogProxyHandler = handlers.NewLogHandlerFunc(*config.LogsProviderURL, config.WriteTimeout)

	functionProxy := faasHandlers.Proxy

	if config.ProbeFunctions {
		prober := probing.NewFunctionProber(cachedFunctionQuery, functionURLResolver)
		// Default of 5 seconds between refreshing probes for function invocations
		probeCache := probing.NewProbeCache(time.Second * 5)
		functionProxy = handlers.MakeProbeHandler(prober, probeCache, functionURLResolver, functionProxy, config.Namespace)
	}

	if config.ScaleFromZero {
		scalingFunctionCache := scaling.NewFunctionCache(scalingConfig.CacheExpiry)
		scaler := scaling.NewFunctionScaler(scalingConfig, scalingFunctionCache)
		functionProxy = handlers.MakeScalingHandler(functionProxy, scaler, scalingConfig, config.Namespace)
	}

	if config.UseNATS() {
		log.Println("Async enabled: Using NATS Streaming.")
		maxReconnect := 60
		interval := time.Second * 2

		defaultNATSConfig := natsHandler.NewDefaultNATSConfig(maxReconnect, interval)

		natsQueue, queueErr := natsHandler.CreateNATSQueue(*config.NATSAddress, *config.NATSPort, *config.NATSClusterName, *config.NATSChannel, defaultNATSConfig)
		if queueErr != nil {
			log.Fatalln(queueErr)
		}

		faasHandlers.QueuedProxy = handlers.MakeNotifierWrapper(
			handlers.MakeCallIDMiddleware(handlers.MakeQueuedProxy(metricsOptions, natsQueue, trimURLTransformer, config.Namespace, cachedFunctionQuery)),
			forwardingNotifiers,
		)
	}

	prometheusQuery := metrics.NewPrometheusQuery(config.PrometheusHost, config.PrometheusPort, &http.Client{})
	faasHandlers.ListFunctions = metrics.AddMetricsHandler(faasHandlers.ListFunctions, prometheusQuery)
	faasHandlers.ScaleFunction = handlers.MakeForwardingProxyHandler(reverseProxy, forwardingNotifiers, urlResolver, nilURLTransformer, serviceAuthInjector)

	if credentials != nil {
		faasHandlers.Alert =
			decorateExternalAuth(faasHandlers.Alert, config.UpstreamTimeout, config.AuthProxyURL, config.AuthProxyPassBody)
		faasHandlers.UpdateFunction =
			decorateExternalAuth(faasHandlers.UpdateFunction, config.UpstreamTimeout, config.AuthProxyURL, config.AuthProxyPassBody)
		faasHandlers.DeleteFunction =
			decorateExternalAuth(faasHandlers.DeleteFunction, config.UpstreamTimeout, config.AuthProxyURL, config.AuthProxyPassBody)
		faasHandlers.DeployFunction =
			decorateExternalAuth(faasHandlers.DeployFunction, config.UpstreamTimeout, config.AuthProxyURL, config.AuthProxyPassBody)
		faasHandlers.ListFunctions =
			decorateExternalAuth(faasHandlers.ListFunctions, config.UpstreamTimeout, config.AuthProxyURL, config.AuthProxyPassBody)
		faasHandlers.ScaleFunction =
			decorateExternalAuth(faasHandlers.ScaleFunction, config.UpstreamTimeout, config.AuthProxyURL, config.AuthProxyPassBody)
		faasHandlers.FunctionStatus =
			decorateExternalAuth(faasHandlers.FunctionStatus, config.UpstreamTimeout, config.AuthProxyURL, config.AuthProxyPassBody)
		faasHandlers.InfoHandler =
			decorateExternalAuth(faasHandlers.InfoHandler, config.UpstreamTimeout, config.AuthProxyURL, config.AuthProxyPassBody)
		faasHandlers.SecretHandler =
			decorateExternalAuth(faasHandlers.SecretHandler, config.UpstreamTimeout, config.AuthProxyURL, config.AuthProxyPassBody)
		faasHandlers.LogProxyHandler =
			decorateExternalAuth(faasHandlers.LogProxyHandler, config.UpstreamTimeout, config.AuthProxyURL, config.AuthProxyPassBody)
		faasHandlers.NamespaceListerHandler =
			decorateExternalAuth(faasHandlers.NamespaceListerHandler, config.UpstreamTimeout, config.AuthProxyURL, config.AuthProxyPassBody)
	}

	r := mux.NewRouter()
	// max wait time to start a function = maxPollCount * functionPollInterval

	r.HandleFunc("/function/{name:["+NameExpression+"]+}", functionProxy)
	r.HandleFunc("/function/{name:["+NameExpression+"]+}/", functionProxy)
	r.HandleFunc("/function/{name:["+NameExpression+"]+}/{params:.*}", functionProxy)

	r.HandleFunc("/system/info", faasHandlers.InfoHandler).Methods(http.MethodGet)
	r.HandleFunc("/system/alert", faasHandlers.Alert).Methods(http.MethodPost)

	r.HandleFunc("/system/function/{name:["+NameExpression+"]+}", faasHandlers.FunctionStatus).Methods(http.MethodGet)
	r.HandleFunc("/system/functions", faasHandlers.ListFunctions).Methods(http.MethodGet)
	r.HandleFunc("/system/functions", faasHandlers.DeployFunction).Methods(http.MethodPost)
	r.HandleFunc("/system/functions", faasHandlers.DeleteFunction).Methods(http.MethodDelete)
	r.HandleFunc("/system/functions", faasHandlers.UpdateFunction).Methods(http.MethodPut)
	r.HandleFunc("/system/scale-function/{name:["+NameExpression+"]+}", faasHandlers.ScaleFunction).Methods(http.MethodPost)

	r.HandleFunc("/system/secrets", faasHandlers.SecretHandler).Methods(http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete)
	r.HandleFunc("/system/logs", faasHandlers.LogProxyHandler).Methods(http.MethodGet)

	r.HandleFunc("/system/namespaces", faasHandlers.NamespaceListerHandler).Methods(http.MethodGet)

	if faasHandlers.QueuedProxy != nil {
		r.HandleFunc("/async-function/{name:["+NameExpression+"]+}/", faasHandlers.QueuedProxy).Methods(http.MethodPost)
		r.HandleFunc("/async-function/{name:["+NameExpression+"]+}", faasHandlers.QueuedProxy).Methods(http.MethodPost)
		r.HandleFunc("/async-function/{name:["+NameExpression+"]+}/{params:.*}", faasHandlers.QueuedProxy).Methods(http.MethodPost)
	}

	fs := http.FileServer(http.Dir("./assets/"))

	// This URL allows access from the UI to the OpenFaaS store
	allowedCORSHost := "raw.githubusercontent.com"
	fsCORS := handlers.DecorateWithCORS(fs, allowedCORSHost)

	uiHandler := http.StripPrefix("/ui", fsCORS)
	if credentials != nil {
		r.PathPrefix("/ui/").Handler(
			decorateExternalAuth(uiHandler.ServeHTTP, config.UpstreamTimeout, config.AuthProxyURL, config.AuthProxyPassBody)).Methods(http.MethodGet)
	} else {
		r.PathPrefix("/ui/").Handler(uiHandler).Methods(http.MethodGet)
	}

	//Start metrics server in a goroutine
	go runMetricsServer()

	r.HandleFunc("/healthz",
		handlers.MakeForwardingProxyHandler(reverseProxy, forwardingNotifiers, urlResolver, nilURLTransformer, serviceAuthInjector)).Methods(http.MethodGet)

	r.Handle("/", http.RedirectHandler("/ui/", http.StatusMovedPermanently)).Methods(http.MethodGet)

	tcpPort := 8080

	s := &http.Server{
		Addr:           fmt.Sprintf(":%d", tcpPort),
		ReadTimeout:    config.ReadTimeout,
		WriteTimeout:   config.WriteTimeout,
		MaxHeaderBytes: http.DefaultMaxHeaderBytes, // 1MB - can be overridden by setting Server.MaxHeaderBytes.
		Handler:        r,
	}

	log.Fatal(s.ListenAndServe())
}

//runMetricsServer Listen on a separate HTTP port for Prometheus metrics to keep this accessible from
// the internal network only.
func runMetricsServer() {
	metricsHandler := metrics.PrometheusHandler()
	router := mux.NewRouter()
	router.Handle("/metrics", metricsHandler)
	router.HandleFunc("/healthz", handlers.HealthzHandler)

	port := 8082
	readTimeout := 5 * time.Second
	writeTimeout := 5 * time.Second

	s := &http.Server{
		Addr:           fmt.Sprintf(":%d", port),
		ReadTimeout:    readTimeout,
		WriteTimeout:   writeTimeout,
		MaxHeaderBytes: http.DefaultMaxHeaderBytes,
		Handler:        router,
	}

	log.Fatal(s.ListenAndServe())
}
