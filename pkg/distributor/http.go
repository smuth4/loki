package distributor

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/httpgrpc"

	"github.com/grafana/loki/v3/pkg/util"

	"github.com/grafana/dskit/tenant"

	"github.com/grafana/loki/v3/pkg/loghttp/push"
	util_log "github.com/grafana/loki/v3/pkg/util/log"
	"github.com/grafana/loki/v3/pkg/validation"
)

// PushHandler reads a snappy-compressed proto from the HTTP body.
func (d *Distributor) PushHandler(w http.ResponseWriter, r *http.Request) {
	d.pushHandler(w, r, push.ParseLokiRequest, push.HTTPError)
}

func (d *Distributor) OTLPPushHandler(w http.ResponseWriter, r *http.Request) {
	d.pushHandler(w, r, push.ParseOTLPRequest, push.OTLPError)
}

func (d *Distributor) pushHandler(w http.ResponseWriter, r *http.Request, pushRequestParser push.RequestParser, errorWriter push.ErrorWriter) {
	logger := util_log.WithContext(r.Context(), util_log.Logger)
	tenantID, err := tenant.TenantID(r.Context())
	if err != nil {
		level.Error(logger).Log("msg", "error getting tenant id", "err", err)
		errorWriter(w, err.Error(), http.StatusBadRequest, logger)
		return
	}

	if d.RequestParserWrapper != nil {
		pushRequestParser = d.RequestParserWrapper(pushRequestParser)
	}

	// Create a request-scoped policy and retention resolver that will ensure consistent policy and retention resolution
	// across all parsers for this HTTP request.
	streamResolver := newRequestScopedStreamResolver(tenantID, d.validator.Limits, logger)

	logPushRequestStreams := d.tenantConfigs.LogPushRequestStreams(tenantID)
	req, err := push.ParseRequest(logger, tenantID, d.cfg.MaxRecvMsgSize, r, d.validator.Limits, pushRequestParser, d.usageTracker, streamResolver, logPushRequestStreams)
	if err != nil {
		switch {
		case errors.Is(err, push.ErrRequestBodyTooLarge):
			if d.tenantConfigs.LogPushRequest(tenantID) {
				level.Debug(logger).Log(
					"msg", "push request failed",
					"code", http.StatusRequestEntityTooLarge,
					"err", err,
				)
			}
			d.writeFailuresManager.Log(tenantID, fmt.Errorf("couldn't decompress push request: %w", err))

			// We count the compressed request body size here
			// because the request body could not be decompressed
			// and thus we don't know the uncompressed size.
			// In addition we don't add the metric label values for
			// `retention_hours` and `policy` because we don't know the labels.
			// Ensure ContentLength is positive to avoid counter panic
			if r.ContentLength > 0 {
				// Add empty values for retention_hours and policy labels since we don't have
				// that information for request body too large errors
				validation.DiscardedBytes.WithLabelValues(validation.RequestBodyTooLarge, tenantID, "", "").Add(float64(r.ContentLength))
			} else {
				level.Error(logger).Log(
					"msg", "negative content length observed",
					"tenantID", tenantID,
					"contentLength", r.ContentLength)
			}
			errorWriter(w, err.Error(), http.StatusRequestEntityTooLarge, logger)
			return

		case !errors.Is(err, push.ErrAllLogsFiltered):
			if d.tenantConfigs.LogPushRequest(tenantID) {
				level.Debug(logger).Log(
					"msg", "push request failed",
					"code", http.StatusBadRequest,
					"err", err,
				)
			}
			d.writeFailuresManager.Log(tenantID, fmt.Errorf("couldn't parse push request: %w", err))

			errorWriter(w, err.Error(), http.StatusBadRequest, logger)
			return

		default:
			if d.tenantConfigs.LogPushRequest(tenantID) {
				level.Debug(logger).Log(
					"msg", "successful push request filtered all lines",
				)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	if logPushRequestStreams {
		var sb strings.Builder
		for _, s := range req.Streams {
			sb.WriteString(s.Labels)
		}
		level.Debug(logger).Log(
			"msg", "push request streams",
			"streams", sb.String(),
		)
	}

	_, err = d.PushWithResolver(r.Context(), req, streamResolver)
	if err == nil {
		if d.tenantConfigs.LogPushRequest(tenantID) {
			level.Debug(logger).Log(
				"msg", "push request successful",
			)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	resp, ok := httpgrpc.HTTPResponseFromError(err)
	if ok {
		body := string(resp.Body)
		if d.tenantConfigs.LogPushRequest(tenantID) {
			level.Debug(logger).Log(
				"msg", "push request failed",
				"code", resp.Code,
				"err", body,
			)
		}
		errorWriter(w, body, int(resp.Code), logger)
	} else {
		if d.tenantConfigs.LogPushRequest(tenantID) {
			level.Debug(logger).Log(
				"msg", "push request failed",
				"code", http.StatusInternalServerError,
				"err", err.Error(),
			)
		}
		errorWriter(w, err.Error(), http.StatusInternalServerError, logger)
	}
}

// ServeHTTP implements the distributor ring status page.
//
// If the rate limiting strategy is local instead of global, no ring is used by
// the distributor and as such, no ring status is returned from this function.
func (d *Distributor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d.rateLimitStrat == validation.GlobalIngestionRateStrategy {
		d.distributorsLifecycler.ServeHTTP(w, r)
		return
	}

	var noRingPage = `
			<!DOCTYPE html>
			<html>
				<head>
					<meta charset="UTF-8">
					<title>Distributor Ring Status</title>
				</head>
				<body>
					<h1>Distributor Ring Status</h1>
					<p>Not running with Global Rating Limit - ring not being used by the Distributor.</p>
				</body>
			</html>`
	util.WriteHTMLResponse(w, noRingPage)
}
