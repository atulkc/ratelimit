package ratelimit_test

import (
	"testing"

	core_legacy "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	pb_struct_legacy "github.com/envoyproxy/go-control-plane/envoy/api/v2/ratelimit"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	pb_struct "github.com/envoyproxy/go-control-plane/envoy/extensions/common/ratelimit/v3"
	pb_legacy "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v2"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/envoyproxy/ratelimit/src/config"
	"github.com/envoyproxy/ratelimit/src/redis"
	"github.com/envoyproxy/ratelimit/src/service"
	"github.com/envoyproxy/ratelimit/test/common"
	"github.com/golang/mock/gomock"
	"github.com/lyft/gostats"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
)

func convertRatelimit(ratelimit *pb.RateLimitResponse_RateLimit) (*pb_legacy.RateLimitResponse_RateLimit, error) {
	if ratelimit == nil {
		return nil, nil
	}

	return &pb_legacy.RateLimitResponse_RateLimit{
		Name:            ratelimit.GetName(),
		RequestsPerUnit: ratelimit.GetRequestsPerUnit(),
		Unit:            pb_legacy.RateLimitResponse_RateLimit_Unit(ratelimit.GetUnit()),
	}, nil
}

func convertRatelimits(ratelimits []*config.RateLimit) ([]*pb_legacy.RateLimitResponse_RateLimit, error) {
	if ratelimits == nil {
		return nil, nil
	}

	ret := make([]*pb_legacy.RateLimitResponse_RateLimit, 0)
	for _, rl := range ratelimits {
		if rl == nil {
			ret = append(ret, nil)
			continue
		}
		legacyRl, err := convertRatelimit(rl.Limit)
		if err != nil {
			return nil, err
		}
		ret = append(ret, legacyRl)
	}

	return ret, nil
}

func TestServiceLegacy(test *testing.T) {
	t := commonSetup(test)
	defer t.controller.Finish()
	service := t.setupBasicService()

	// First request, config should be loaded.
	legacyRequest := common.NewRateLimitRequestLegacy("test-domain", [][][2]string{{{"hello", "world"}}}, 1)
	req, err := ratelimit.ConvertLegacyRequest(legacyRequest)
	if err != nil {
		t.assert.FailNow(err.Error())
	}
	t.config.EXPECT().GetLimit(nil, "test-domain", req.Descriptors[0]).Return(nil)
	t.cache.EXPECT().DoLimit(nil, req, []*config.RateLimit{nil}).Return(
		[]*pb.RateLimitResponse_DescriptorStatus{{Code: pb.RateLimitResponse_OK, CurrentLimit: nil, LimitRemaining: 0}})

	response, err := service.GetLegacyService().ShouldRateLimit(nil, legacyRequest)
	common.AssertProtoEqual(
		t.assert,
		&pb_legacy.RateLimitResponse{
			OverallCode: pb_legacy.RateLimitResponse_OK,
			Statuses:    []*pb_legacy.RateLimitResponse_DescriptorStatus{{Code: pb_legacy.RateLimitResponse_OK, CurrentLimit: nil, LimitRemaining: 0}}},
		response)
	t.assert.Nil(err)

	// Force a config reload.
	barrier := newBarrier()
	t.configLoader.EXPECT().Load(
		[]config.RateLimitConfigToLoad{{"config.basic_config", "fake_yaml"}}, gomock.Any()).Do(
		func([]config.RateLimitConfigToLoad, stats.Scope) { barrier.signal() }).Return(t.config)
	t.runtimeUpdateCallback <- 1
	barrier.wait()

	// Different request.
	legacyRequest = common.NewRateLimitRequestLegacy(
		"different-domain", [][][2]string{{{"foo", "bar"}}, {{"hello", "world"}}}, 1)
	req, err = ratelimit.ConvertLegacyRequest(legacyRequest)
	if err != nil {
		t.assert.FailNow(err.Error())
	}

	limits := []*config.RateLimit{
		config.NewRateLimit(10, pb.RateLimitResponse_RateLimit_MINUTE, "key", t.statStore),
		nil}
	legacyLimits, err := convertRatelimits(limits)
	if err != nil {
		t.assert.FailNow(err.Error())
	}

	t.config.EXPECT().GetLimit(nil, "different-domain", req.Descriptors[0]).Return(limits[0])
	t.config.EXPECT().GetLimit(nil, "different-domain", req.Descriptors[1]).Return(limits[1])
	t.cache.EXPECT().DoLimit(nil, req, limits).Return(
		[]*pb.RateLimitResponse_DescriptorStatus{{Code: pb.RateLimitResponse_OVER_LIMIT, CurrentLimit: limits[0].Limit, LimitRemaining: 0},
			{Code: pb.RateLimitResponse_OK, CurrentLimit: nil, LimitRemaining: 0}})
	response, err = service.GetLegacyService().ShouldRateLimit(nil, legacyRequest)
	common.AssertProtoEqual(
		t.assert,
		&pb_legacy.RateLimitResponse{
			OverallCode: pb_legacy.RateLimitResponse_OVER_LIMIT,
			Statuses: []*pb_legacy.RateLimitResponse_DescriptorStatus{
				{Code: pb_legacy.RateLimitResponse_OVER_LIMIT, CurrentLimit: legacyLimits[0], LimitRemaining: 0},
				{Code: pb_legacy.RateLimitResponse_OK, CurrentLimit: nil, LimitRemaining: 0},
			}},
		response)
	t.assert.Nil(err)

	// Config load failure.
	t.configLoader.EXPECT().Load(
		[]config.RateLimitConfigToLoad{{"config.basic_config", "fake_yaml"}}, gomock.Any()).Do(
		func([]config.RateLimitConfigToLoad, stats.Scope) {
			defer barrier.signal()
			panic(config.RateLimitConfigError("load error"))
		})
	t.runtimeUpdateCallback <- 1
	barrier.wait()

	// Config should still be valid. Also make sure order does not affect results.
	limits = []*config.RateLimit{
		nil,
		config.NewRateLimit(10, pb.RateLimitResponse_RateLimit_MINUTE, "key", t.statStore)}
	legacyLimits, err = convertRatelimits(limits)
	if err != nil {
		t.assert.FailNow(err.Error())
	}

	t.config.EXPECT().GetLimit(nil, "different-domain", req.Descriptors[0]).Return(limits[0])
	t.config.EXPECT().GetLimit(nil, "different-domain", req.Descriptors[1]).Return(limits[1])
	t.cache.EXPECT().DoLimit(nil, req, limits).Return(
		[]*pb.RateLimitResponse_DescriptorStatus{{Code: pb.RateLimitResponse_OK, CurrentLimit: nil, LimitRemaining: 0},
			{Code: pb.RateLimitResponse_OVER_LIMIT, CurrentLimit: limits[1].Limit, LimitRemaining: 0}})
	response, err = service.GetLegacyService().ShouldRateLimit(nil, legacyRequest)
	common.AssertProtoEqual(
		t.assert,
		&pb_legacy.RateLimitResponse{
			OverallCode: pb_legacy.RateLimitResponse_OVER_LIMIT,
			Statuses: []*pb_legacy.RateLimitResponse_DescriptorStatus{
				{Code: pb_legacy.RateLimitResponse_OK, CurrentLimit: nil, LimitRemaining: 0},
				{Code: pb_legacy.RateLimitResponse_OVER_LIMIT, CurrentLimit: legacyLimits[1], LimitRemaining: 0},
			}},
		response)
	t.assert.Nil(err)

	t.assert.EqualValues(2, t.statStore.NewCounter("config_load_success").Value())
	t.assert.EqualValues(1, t.statStore.NewCounter("config_load_error").Value())
}

func TestEmptyDomainLegacy(test *testing.T) {
	t := commonSetup(test)
	defer t.controller.Finish()
	service := t.setupBasicService()

	request := common.NewRateLimitRequestLegacy("", [][][2]string{{{"hello", "world"}}}, 1)
	response, err := service.GetLegacyService().ShouldRateLimit(nil, request)
	t.assert.Nil(response)
	t.assert.Equal("rate limit domain must not be empty", err.Error())
	t.assert.EqualValues(1, t.statStore.NewCounter("call.should_rate_limit.service_error").Value())
	t.assert.EqualValues(1, t.statStore.NewCounter("call.should_rate_limit_legacy.should_rate_limit_error").Value())
}

func TestEmptyDescriptorsLegacy(test *testing.T) {
	t := commonSetup(test)
	defer t.controller.Finish()
	service := t.setupBasicService()

	request := common.NewRateLimitRequestLegacy("test-domain", [][][2]string{}, 1)
	response, err := service.GetLegacyService().ShouldRateLimit(nil, request)
	t.assert.Nil(response)
	t.assert.Equal("rate limit descriptor list must not be empty", err.Error())
	t.assert.EqualValues(1, t.statStore.NewCounter("call.should_rate_limit.service_error").Value())
	t.assert.EqualValues(1, t.statStore.NewCounter("call.should_rate_limit_legacy.should_rate_limit_error").Value())
}

func TestCacheErrorLegacy(test *testing.T) {
	t := commonSetup(test)
	defer t.controller.Finish()
	service := t.setupBasicService()

	legacyRequest := common.NewRateLimitRequestLegacy("different-domain", [][][2]string{{{"foo", "bar"}}}, 1)
	req, err := ratelimit.ConvertLegacyRequest(legacyRequest)
	if err != nil {
		t.assert.FailNow(err.Error())
	}
	limits := []*config.RateLimit{config.NewRateLimit(10, pb.RateLimitResponse_RateLimit_MINUTE, "key", t.statStore)}
	t.config.EXPECT().GetLimit(nil, "different-domain", req.Descriptors[0]).Return(limits[0])
	t.cache.EXPECT().DoLimit(nil, req, limits).Do(
		func(context.Context, *pb.RateLimitRequest, []*config.RateLimit) {
			panic(redis.RedisError("cache error"))
		})

	response, err := service.GetLegacyService().ShouldRateLimit(nil, legacyRequest)
	t.assert.Nil(response)
	t.assert.Equal("cache error", err.Error())
	t.assert.EqualValues(1, t.statStore.NewCounter("call.should_rate_limit.redis_error").Value())
	t.assert.EqualValues(1, t.statStore.NewCounter("call.should_rate_limit_legacy.should_rate_limit_error").Value())
}

func TestInitialLoadErrorLegacy(test *testing.T) {
	t := commonSetup(test)
	defer t.controller.Finish()

	t.runtime.EXPECT().AddUpdateCallback(gomock.Any()).Do(
		func(callback chan<- int) { t.runtimeUpdateCallback = callback })
	t.runtime.EXPECT().Snapshot().Return(t.snapshot).MinTimes(1)
	t.snapshot.EXPECT().Keys().Return([]string{"foo", "config.basic_config"}).MinTimes(1)
	t.snapshot.EXPECT().Get("config.basic_config").Return("fake_yaml").MinTimes(1)
	t.configLoader.EXPECT().Load(
		[]config.RateLimitConfigToLoad{{"config.basic_config", "fake_yaml"}}, gomock.Any()).Do(
		func([]config.RateLimitConfigToLoad, stats.Scope) {
			panic(config.RateLimitConfigError("load error"))
		})
	service := ratelimit.NewService(t.runtime, t.cache, t.configLoader, t.statStore, true)

	request := common.NewRateLimitRequestLegacy("test-domain", [][][2]string{{{"hello", "world"}}}, 1)
	response, err := service.GetLegacyService().ShouldRateLimit(nil, request)
	t.assert.Nil(response)
	t.assert.Equal("no rate limit configuration loaded", err.Error())
	t.assert.EqualValues(1, t.statStore.NewCounter("call.should_rate_limit.service_error").Value())
	t.assert.EqualValues(1, t.statStore.NewCounter("call.should_rate_limit_legacy.should_rate_limit_error").Value())

}

func TestConvertLegacyRequest(test *testing.T) {
	req, err := ratelimit.ConvertLegacyRequest(nil)
	if err != nil {
		assert.FailNow(test, err.Error())
	}
	assert.Nil(test, req)

	{
		request := &pb_legacy.RateLimitRequest{
			Domain:      "test",
			Descriptors: nil,
			HitsAddend:  10,
		}

		expectedRequest := &pb.RateLimitRequest{
			Domain:      "test",
			Descriptors: nil,
			HitsAddend:  10,
		}

		req, err := ratelimit.ConvertLegacyRequest(request)
		if err != nil {
			assert.FailNow(test, err.Error())
		}

		common.AssertProtoEqual(assert.New(test), expectedRequest, req)
	}

	{
		request := &pb_legacy.RateLimitRequest{
			Domain:      "test",
			Descriptors: []*pb_struct_legacy.RateLimitDescriptor{},
			HitsAddend:  10,
		}

		expectedRequest := &pb.RateLimitRequest{
			Domain:      "test",
			Descriptors: []*pb_struct.RateLimitDescriptor{},
			HitsAddend:  10,
		}

		req, err := ratelimit.ConvertLegacyRequest(request)
		if err != nil {
			assert.FailNow(test, err.Error())
		}

		common.AssertProtoEqual(assert.New(test), expectedRequest, req)
	}

	{
		descriptors := []*pb_struct_legacy.RateLimitDescriptor{
			{
				Entries: []*pb_struct_legacy.RateLimitDescriptor_Entry{
					{
						Key:   "foo",
						Value: "foo_value",
					},
					nil,
				},
			},
			{
				Entries: []*pb_struct_legacy.RateLimitDescriptor_Entry{},
			},
			{
				Entries: nil,
			},
			nil,
		}

		request := &pb_legacy.RateLimitRequest{
			Domain:      "test",
			Descriptors: descriptors,
			HitsAddend:  10,
		}

		expectedDescriptors := []*pb_struct.RateLimitDescriptor{
			{
				Entries: []*pb_struct.RateLimitDescriptor_Entry{
					{
						Key:   "foo",
						Value: "foo_value",
					},
					nil,
				},
			},
			{
				Entries: []*pb_struct.RateLimitDescriptor_Entry{},
			},
			{
				Entries: nil,
			},
			nil,
		}

		expectedRequest := &pb.RateLimitRequest{
			Domain:      "test",
			Descriptors: expectedDescriptors,
			HitsAddend:  10,
		}

		req, err := ratelimit.ConvertLegacyRequest(request)
		if err != nil {
			assert.FailNow(test, err.Error())
		}

		common.AssertProtoEqual(assert.New(test), expectedRequest, req)
	}
}

func TestConvertResponse(test *testing.T) {
	resp, err := ratelimit.ConvertResponse(nil)
	if err != nil {
		assert.FailNow(test, err.Error())
	}
	assert.Nil(test, resp)

	rl := &pb.RateLimitResponse_RateLimit{
		RequestsPerUnit: 10,
		Unit:            pb.RateLimitResponse_RateLimit_DAY,
	}

	statuses := []*pb.RateLimitResponse_DescriptorStatus{
		{
			Code:           pb.RateLimitResponse_OK,
			CurrentLimit:   nil,
			LimitRemaining: 9,
		},
		nil,
		{
			Code:           pb.RateLimitResponse_OVER_LIMIT,
			CurrentLimit:   rl,
			LimitRemaining: 0,
		},
	}

	requestHeadersToAdd := []*core.HeaderValue{{
		Key:   "test_request",
		Value: "test_request_value",
	}, nil}

	responseHeadersToAdd := []*core.HeaderValue{{
		Key:   "test_response",
		Value: "test_response",
	}, nil}

	response := &pb.RateLimitResponse{
		OverallCode:          pb.RateLimitResponse_OVER_LIMIT,
		Statuses:             statuses,
		RequestHeadersToAdd:  requestHeadersToAdd,
		ResponseHeadersToAdd: responseHeadersToAdd,
	}

	expectedRl := &pb_legacy.RateLimitResponse_RateLimit{
		RequestsPerUnit: 10,
		Unit:            pb_legacy.RateLimitResponse_RateLimit_DAY,
	}

	expectedStatuses := []*pb_legacy.RateLimitResponse_DescriptorStatus{
		{
			Code:           pb_legacy.RateLimitResponse_OK,
			CurrentLimit:   nil,
			LimitRemaining: 9,
		},
		nil,
		{
			Code:           pb_legacy.RateLimitResponse_OVER_LIMIT,
			CurrentLimit:   expectedRl,
			LimitRemaining: 0,
		},
	}

	expectedRequestHeadersToAdd := []*core_legacy.HeaderValue{{
		Key:   "test_request",
		Value: "test_request_value",
	}, nil}

	expecpectedResponseHeadersToAdd := []*core_legacy.HeaderValue{{
		Key:   "test_response",
		Value: "test_response",
	}, nil}

	expectedResponse := &pb_legacy.RateLimitResponse{
		OverallCode:         pb_legacy.RateLimitResponse_OVER_LIMIT,
		Statuses:            expectedStatuses,
		RequestHeadersToAdd: expectedRequestHeadersToAdd,
		Headers:             expecpectedResponseHeadersToAdd,
	}

	resp, err = ratelimit.ConvertResponse(response)
	if err != nil {
		assert.FailNow(test, err.Error())
	}

	common.AssertProtoEqual(assert.New(test), expectedResponse, resp)
}
