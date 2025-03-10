/*
Copyright 2022 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package http

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/valyala/fasthttp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/dapr/dapr/pkg/messages"
)

// Object containing options for the UniversalFastHTTPHandler method.
type UniversalFastHTTPHandlerOpts[T proto.Message, U proto.Message] struct {
	// This modifier allows modifying the input proto object before the handler is called. This property is optional.
	// The input proto object contantains all properties parsed from the request's body (for non-GET requests), and this modifier can alter it for example with properties from the URL (to make APIs RESTful).
	// The modifier should return the modified object.
	InModifier func(reqCtx *fasthttp.RequestCtx, in T) (T, error)

	// This modifier allows modifying the output proto object before the response is sent to the client. This property is optional.
	// This is primarily meant to ensure that existing APIs can be migrated to Universal ones while preserving the same response in case of small differences.
	// The response could be a proto object (which will be serialized with protojson) or any other object (serialized with the standard JSON package). If the response is nil, a 204 (no content) response is sent to the client, with no data in the body.
	// NOTE: Newly-implemented APIs should ensure that on the HTTP endpoint the response matches the protos to offer a consistent experience, and should NOT modify the output before it's sent to the client.
	OutModifier func(out U) (any, error)
}

// UniversalFastHTTPHandler wraps a UniversalAPI method into a FastHTTP handler.
func UniversalFastHTTPHandler[T proto.Message, U proto.Message](
	handler func(ctx context.Context, in T) (U, error),
	opts UniversalFastHTTPHandlerOpts[T, U],
) fasthttp.RequestHandler {
	var zero T
	rt := reflect.ValueOf(zero).Type().Elem()
	pjsonDec := protojson.UnmarshalOptions{
		DiscardUnknown: true,
		AllowPartial:   true,
	}

	return func(reqCtx *fasthttp.RequestCtx) {
		// Read the response body and decode it as JSON using protojson
		body := reqCtx.PostBody()
		// Need to use some reflection magic to allocate a value for the pointer of the generic type T
		in := reflect.New(rt).Interface().(T)
		if len(body) > 0 {
			err := pjsonDec.Unmarshal(body, in)
			if err != nil {
				msg := NewErrorResponse("ERR_MALFORMED_REQUEST", err.Error())
				respond(reqCtx, withError(fasthttp.StatusBadRequest, msg))
				log.Debug(msg)
				return
			}
		}

		var err error

		// If we have an inModifier function, invoke it now
		if opts.InModifier != nil {
			in, err = opts.InModifier(reqCtx, in)
			if err != nil {
				universalFastHTTPErrorResponder(reqCtx, err)
				return
			}
		}

		// Invoke the gRPC handler
		res, err := handler(reqCtx, in)
		if err != nil {
			// Error is already logged by the handlers, we won't log it again
			universalFastHTTPErrorResponder(reqCtx, err)
			return
		}

		if reflect.ValueOf(res).IsZero() {
			respond(reqCtx, withEmpty())
			return
		}

		// If we do not have an output modifier, respond right away
		if opts.OutModifier == nil {
			universalFastHTTPProtoResponder(reqCtx, res)
			return
		}

		// Invoke the modifier
		newRes, err := opts.OutModifier(res)
		if err != nil {
			universalFastHTTPErrorResponder(reqCtx, err)
			return
		}

		// Check if the response is a proto object (which is encoded with protojson) or any other object to encode as regular JSON
		switch m := newRes.(type) {
		case nil:
			respond(reqCtx, withEmpty())
			return
		case protoreflect.ProtoMessage:
			universalFastHTTPProtoResponder(reqCtx, m)
			return
		default:
			universalFastHTTPJSONResponder(reqCtx, m)
			return
		}
	}
}

func universalFastHTTPProtoResponder(reqCtx *fasthttp.RequestCtx, m protoreflect.ProtoMessage) {
	// Encode the response as JSON using protojson
	respBytes, err := protojson.Marshal(m)
	if err != nil {
		msg := NewErrorResponse("ERR_INTERNAL", "failed to encode response as JSON: "+err.Error())
		respond(reqCtx, withError(fasthttp.StatusInternalServerError, msg))
		log.Debug(msg)
		return
	}

	respond(reqCtx, withJSON(fasthttp.StatusOK, respBytes))
}

func universalFastHTTPJSONResponder(reqCtx *fasthttp.RequestCtx, m any) {
	// Encode the response as JSON using the regular JSON package
	respBytes, err := json.Marshal(m)
	if err != nil {
		msg := NewErrorResponse("ERR_INTERNAL", "failed to encode response as JSON: "+err.Error())
		respond(reqCtx, withError(fasthttp.StatusInternalServerError, msg))
		log.Debug(msg)
		return
	}

	respond(reqCtx, withJSON(fasthttp.StatusOK, respBytes))
}

func universalFastHTTPErrorResponder(reqCtx *fasthttp.RequestCtx, err error) {
	if err == nil {
		return
	}

	// Check if it's an APIError object
	apiErr, ok := err.(messages.APIError)
	if ok {
		msg := NewErrorResponse(apiErr.Tag(), apiErr.Message())
		respond(reqCtx, withError(apiErr.HTTPCode(), msg))
		return
	}

	// Respond with a generic error
	msg := NewErrorResponse("ERROR", err.Error())
	respond(reqCtx, withError(fasthttp.StatusInternalServerError, msg))
}
