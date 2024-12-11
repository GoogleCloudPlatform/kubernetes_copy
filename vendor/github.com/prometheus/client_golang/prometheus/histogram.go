// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheus

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	dto "github.com/prometheus/client_model/go"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// nativeHistogramBounds for the frac of observed values. Only relevant for
// schema > 0. The position in the slice is the schema. (0 is never used, just
// here for convenience of using the schema directly as the index.)
//
// TODO(beorn7): Currently, we do a binary search into these slices. There are
// ways to turn it into a small number of simple array lookups. It probably only
// matters for schema 5 and beyond, but should be investigated. See this comment
// as a starting point:
// https://github.com/open-telemetry/opentelemetry-specification/issues/1776#issuecomment-870164310
var nativeHistogramBounds = [][]float64{
	// Schema "0":
	{0.5},
	// Schema 1:
	{0.5, 0.7071067811865475},
	// Schema 2:
	{0.5, 0.5946035575013605, 0.7071067811865475, 0.8408964152537144},
	// Schema 3:
	{
		0.5, 0.5452538663326288, 0.5946035575013605, 0.6484197773255048,
		0.7071067811865475, 0.7711054127039704, 0.8408964152537144, 0.9170040432046711,
	},
	// Schema 4:
	{
		0.5, 0.5221368912137069, 0.5452538663326288, 0.5693943173783458,
		0.5946035575013605, 0.620928906036742, 0.6484197773255048, 0.6771277734684463,
		0.7071067811865475, 0.7384130729697496, 0.7711054127039704, 0.805245165974627,
		0.8408964152537144, 0.8781260801866495, 0.9170040432046711, 0.9576032806985735,
	},
	// Schema 5:
	{
		0.5, 0.5109485743270583, 0.5221368912137069, 0.5335702003384117,
		0.5452538663326288, 0.5571933712979462, 0.5693943173783458, 0.5818624293887887,
		0.5946035575013605, 0.6076236799902344, 0.620928906036742, 0.6345254785958666,
		0.6484197773255048, 0.6626183215798706, 0.6771277734684463, 0.6919549409819159,
		0.7071067811865475, 0.7225904034885232, 0.7384130729697496, 0.7545822137967112,
		0.7711054127039704, 0.7879904225539431, 0.805245165974627, 0.8228777390769823,
		0.8408964152537144, 0.8593096490612387, 0.8781260801866495, 0.8973545375015533,
		0.9170040432046711, 0.9370838170551498, 0.9576032806985735, 0.9785720620876999,
	},
	// Schema 6:
	{
		0.5, 0.5054446430258502, 0.5109485743270583, 0.5165124395106142,
		0.5221368912137069, 0.5278225891802786, 0.5335702003384117, 0.5393803988785598,
		0.5452538663326288, 0.5511912916539204, 0.5571933712979462, 0.5632608093041209,
		0.5693943173783458, 0.5755946149764913, 0.5818624293887887, 0.5881984958251406,
		0.5946035575013605, 0.6010783657263515, 0.6076236799902344, 0.6142402680534349,
		0.620928906036742, 0.6276903785123455, 0.6345254785958666, 0.6414350080393891,
		0.6484197773255048, 0.6554806057623822, 0.6626183215798706, 0.6698337620266515,
		0.6771277734684463, 0.6845012114872953, 0.6919549409819159, 0.6994898362691555,
		0.7071067811865475, 0.7148066691959849, 0.7225904034885232, 0.7304588970903234,
		0.7384130729697496, 0.7464538641456323, 0.7545822137967112, 0.762799075372269,
		0.7711054127039704, 0.7795022001189185, 0.7879904225539431, 0.7965710756711334,
		0.805245165974627, 0.8140137109286738, 0.8228777390769823, 0.8318382901633681,
		0.8408964152537144, 0.8500531768592616, 0.8593096490612387, 0.8686669176368529,
		0.8781260801866495, 0.8876882462632604, 0.8973545375015533, 0.9071260877501991,
		0.9170040432046711, 0.9269895625416926, 0.9370838170551498, 0.9472879907934827,
		0.9576032806985735, 0.9680308967461471, 0.9785720620876999, 0.9892280131939752,
	},
	// Schema 7:
	{
		0.5, 0.5027149505564014, 0.5054446430258502, 0.5081891574554764,
		0.5109485743270583, 0.5137229745593818, 0.5165124395106142, 0.5193170509806894,
		0.5221368912137069, 0.5249720429003435, 0.5278225891802786, 0.5306886136446309,
		0.5335702003384117, 0.5364674337629877, 0.5393803988785598, 0.5423091811066545,
		0.5452538663326288, 0.5482145409081883, 0.5511912916539204, 0.5541842058618393,
		0.5571933712979462, 0.5602188762048033, 0.5632608093041209, 0.5663192597993595,
		0.5693943173783458, 0.572486072215902, 0.5755946149764913, 0.5787200368168754,
		0.5818624293887887, 0.585021884841625, 0.5881984958251406, 0.5913923554921704,
		0.5946035575013605, 0.5978321960199137, 0.6010783657263515, 0.6043421618132907,
		0.6076236799902344, 0.6109230164863786, 0.6142402680534349, 0.6175755319684665,
		0.620928906036742, 0.6243004885946023, 0.6276903785123455, 0.6310986751971253,
		0.6345254785958666, 0.637970889198196, 0.6414350080393891, 0.6449179367033329,
		0.6484197773255048, 0.6519406325959679, 0.6554806057623822, 0.659039800633032,
		0.6626183215798706, 0.6662162735415805, 0.6698337620266515, 0.6734708931164728,
		0.6771277734684463, 0.6808045103191123, 0.6845012114872953, 0.688217985377265,
		0.6919549409819159, 0.6957121878859629, 0.6994898362691555, 0.7032879969095076,
		0.7071067811865475, 0.7109463010845827, 0.7148066691959849, 0.718687998724491,
		0.7225904034885232, 0.7265139979245261, 0.7304588970903234, 0.7344252166684908,
		0.7384130729697496, 0.7424225829363761, 0.7464538641456323, 0.7505070348132126,
		0.7545822137967112, 0.7586795205991071, 0.762799075372269, 0.7669409989204777,
		0.7711054127039704, 0.7752924388424999, 0.7795022001189185, 0.7837348199827764,
		0.7879904225539431, 0.7922691326262467, 0.7965710756711334, 0.8008963778413465,
		0.805245165974627, 0.8096175675974316, 0.8140137109286738, 0.8184337248834821,
		0.8228777390769823, 0.8273458838280969, 0.8318382901633681, 0.8363550898207981,
		0.8408964152537144, 0.8454623996346523, 0.8500531768592616, 0.8546688815502312,
		0.8593096490612387, 0.8639756154809185, 0.8686669176368529, 0.8733836930995842,
		0.8781260801866495, 0.8828942179666361, 0.8876882462632604, 0.8925083056594671,
		0.8973545375015533, 0.9022270839033115, 0.9071260877501991, 0.9120516927035263,
		0.9170040432046711, 0.9219832844793128, 0.9269895625416926, 0.9320230241988943,
		0.9370838170551498, 0.9421720895161669, 0.9472879907934827, 0.9524316709088368,
		0.9576032806985735, 0.9628029718180622, 0.9680308967461471, 0.9732872087896164,
		0.9785720620876999, 0.9838856116165875, 0.9892280131939752, 0.9945994234836328,
	},
	// Schema 8:
	{
		0.5, 0.5013556375251013, 0.5027149505564014, 0.5040779490592088,
		0.5054446430258502, 0.5068150424757447, 0.5081891574554764, 0.509566998038869,
		0.5109485743270583, 0.5123338964485679, 0.5137229745593818, 0.5151158188430205,
		0.5165124395106142, 0.5179128468009786, 0.5193170509806894, 0.520725062344158,
		0.5221368912137069, 0.5235525479396449, 0.5249720429003435, 0.526395386502313,
		0.5278225891802786, 0.5292536613972564, 0.5306886136446309, 0.5321274564422321,
		0.5335702003384117, 0.5350168559101208, 0.5364674337629877, 0.5379219445313954,
		0.5393803988785598, 0.5408428074966075, 0.5423091811066545, 0.5437795304588847,
		0.5452538663326288, 0.5467321995364429, 0.5482145409081883, 0.549700901315111,
		0.5511912916539204, 0.5526857228508706, 0.5541842058618393, 0.5556867516724088,
		0.5571933712979462, 0.5587040757836845, 0.5602188762048033, 0.5617377836665098,
		0.5632608093041209, 0.564787964283144, 0.5663192597993595, 0.5678547070789026,
		0.5693943173783458, 0.5709381019847808, 0.572486072215902, 0.5740382394200894,
		0.5755946149764913, 0.5771552102951081, 0.5787200368168754, 0.5802891060137493,
		0.5818624293887887, 0.5834400184762408, 0.585021884841625, 0.5866080400818185,
		0.5881984958251406, 0.5897932637314379, 0.5913923554921704, 0.5929957828304968,
		0.5946035575013605, 0.5962156912915756, 0.5978321960199137, 0.5994530835371903,
		0.6010783657263515, 0.6027080545025619, 0.6043421618132907, 0.6059806996384005,
		0.6076236799902344, 0.6092711149137041, 0.6109230164863786, 0.6125793968185725,
		0.6142402680534349, 0.6159056423670379, 0.6175755319684665, 0.6192499490999082,
		0.620928906036742, 0.622612415087629, 0.6243004885946023, 0.6259931389331581,
		0.6276903785123455, 0.6293922197748583, 0.6310986751971253, 0.6328097572894031,
		0.6345254785958666, 0.6362458516947014, 0.637970889198196, 0.6397006037528346,
		0.6414350080393891, 0.6431741147730128, 0.6449179367033329, 0.6466664866145447,
		0.6484197773255048, 0.6501778216898253, 0.6519406325959679, 0.6537082229673385,
		0.6554806057623822, 0.6572577939746774, 0.659039800633032, 0.6608266388015788,
		0.6626183215798706, 0.6644148621029772, 0.6662162735415805, 0.6680225691020727,
		0.6698337620266515, 0.6716498655934177, 0.6734708931164728, 0.6752968579460171,
		0.6771277734684463, 0.6789636531064505, 0.6808045103191123, 0.6826503586020058,
		0.6845012114872953, 0.6863570825438342, 0.688217985377265, 0.690083933630119,
		0.6919549409819159, 0.6938310211492645, 0.6957121878859629, 0.6975984549830999,
		0.6994898362691555, 0.7013863456101023, 0.7032879969095076, 0.7051948041086352,
		0.7071067811865475, 0.7090239421602076, 0.7109463010845827, 0.7128738720527471,
		0.7148066691959849, 0.7167447066838943, 0.718687998724491, 0.7206365595643126,
		0.7225904034885232, 0.7245495448210174, 0.7265139979245261, 0.7284837772007218,
		0.7304588970903234, 0.7324393720732029, 0.7344252166684908, 0.7364164454346837,
		0.7384130729697496, 0.7404151139112358, 0.7424225829363761, 0.7444354947621984,
		0.7464538641456323, 0.7484777058836176, 0.7505070348132126, 0.7525418658117031,
		0.7545822137967112, 0.7566280937263048, 0.7586795205991071, 0.7607365094544071,
		0.762799075372269, 0.7648672334736434, 0.7669409989204777, 0.7690203869158282,
		0.7711054127039704, 0.7731960915705107, 0.7752924388424999, 0.7773944698885442,
		0.7795022001189185, 0.7816156449856788, 0.7837348199827764, 0.7858597406461707,
		0.7879904225539431, 0.7901268813264122, 0.7922691326262467, 0.7944171921585818,
		0.7965710756711334, 0.7987307989543135, 0.8008963778413465, 0.8030678282083853,
		0.805245165974627, 0.8074284071024302, 0.8096175675974316, 0.8118126635086642,
		0.8140137109286738, 0.8162207259936375, 0.8184337248834821, 0.820652723822003,
		0.8228777390769823, 0.8251087869603088, 0.8273458838280969, 0.8295890460808079,
		0.8318382901633681, 0.8340936325652911, 0.8363550898207981, 0.8386226785089391,
		0.8408964152537144, 0.8431763167241966, 0.8454623996346523, 0.8477546807446661,
		0.8500531768592616, 0.8523579048290255, 0.8546688815502312, 0.8569861239649629,
		0.8593096490612387, 0.8616394738731368, 0.8639756154809185, 0.8663180910111553,
		0.8686669176368529, 0.871022112577578, 0.8733836930995842, 0.8757516765159389,
		0.8781260801866495, 0.8805069215187917, 0.8828942179666361, 0.8852879870317771,
		0.8876882462632604, 0.890095013257712, 0.8925083056594671, 0.8949281411607002,
		0.8973545375015533, 0.8997875124702672, 0.9022270839033115, 0.9046732696855155,
		0.9071260877501991, 0.909585556079304, 0.9120516927035263, 0.9145245157024483,
		0.9170040432046711, 0.9194902933879467, 0.9219832844793128, 0.9244830347552253,
		0.9269895625416926, 0.92950288621441, 0.9320230241988943, 0.9345499949706191,
		0.9370838170551498, 0.93962450902828, 0.9421720895161669, 0.9447265771954693,
		0.9472879907934827, 0.9498563490882775, 0.9524316709088368, 0.9550139751351947,
		0.9576032806985735, 0.9601996065815236, 0.9628029718180622, 0.9654133954938133,
		0.9680308967461471, 0.9706554947643201, 0.9732872087896164, 0.9759260581154889,
		0.9785720620876999, 0.9812252401044634, 0.9838856116165875, 0.9865531961276168,
		0.9892280131939752, 0.9919100824251095, 0.9945994234836328, 0.9972960560854698,
	},
}

// The nativeHistogramBounds above can be generated with the code below.
//
// TODO(beorn7): It's tempting to actually use `go generate` to generate the
// code above. However, this could lead to slightly different numbers on
// different architectures. We still need to come to terms if we are fine with
// that, or if we might prefer to specify precise numbers in the standard.
//
// var nativeHistogramBounds [][]float64 = make([][]float64, 9)
//
// func init() {
// 	// Populate nativeHistogramBounds.
// 	numBuckets := 1
// 	for i := range nativeHistogramBounds {
// 		bounds := []float64{0.5}
// 		factor := math.Exp2(math.Exp2(float64(-i)))
// 		for j := 0; j < numBuckets-1; j++ {
// 			var bound float64
// 			if (j+1)%2 == 0 {
// 				// Use previously calculated value for increased precision.
// 				bound = nativeHistogramBounds[i-1][j/2+1]
// 			} else {
// 				bound = bounds[j] * factor
// 			}
// 			bounds = append(bounds, bound)
// 		}
// 		numBuckets *= 2
// 		nativeHistogramBounds[i] = bounds
// 	}
// }

// A Histogram counts individual observations from an event or sample stream in
// configurable static buckets (or in dynamic sparse buckets as part of the
// experimental Native Histograms, see below for more details). Similar to a
// Summary, it also provides a sum of observations and an observation count.
//
// On the Prometheus server, quantiles can be calculated from a Histogram using
// the histogram_quantile PromQL function.
//
// Note that Histograms, in contrast to Summaries, can be aggregated in PromQL
// (see the documentation for detailed procedures). However, Histograms require
// the user to pre-define suitable buckets, and they are in general less
// accurate. (Both problems are addressed by the experimental Native
// Histograms. To use them, configure a NativeHistogramBucketFactor in the
// HistogramOpts. They also require a Prometheus server v2.40+ with the
// corresponding feature flag enabled.)
//
// The Observe method of a Histogram has a very low performance overhead in
// comparison with the Observe method of a Summary.
//
// To create Histogram instances, use NewHistogram.
type Histogram interface {
	Metric
	Collector

	// Observe adds a single observation to the histogram. Observations are
	// usually positive or zero. Negative observations are accepted but
	// prevent current versions of Prometheus from properly detecting
	// counter resets in the sum of observations. (The experimental Native
	// Histograms handle negative observations properly.) See
	// https://prometheus.io/docs/practices/histograms/#count-and-sum-of-observations
	// for details.
	Observe(float64)
}

// bucketLabel is used for the label that defines the upper bound of a
// bucket of a histogram ("le" -> "less or equal").
const bucketLabel = "le"

// DefBuckets are the default Histogram buckets. The default buckets are
// tailored to broadly measure the response time (in seconds) of a network
// service. Most likely, however, you will be required to define buckets
// customized to your use case.
var DefBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// DefNativeHistogramZeroThreshold is the default value for
// NativeHistogramZeroThreshold in the HistogramOpts.
//
// The value is 2^-128 (or 0.5*2^-127 in the actual IEEE 754 representation),
// which is a bucket boundary at all possible resolutions.
const DefNativeHistogramZeroThreshold = 2.938735877055719e-39

// NativeHistogramZeroThresholdZero can be used as NativeHistogramZeroThreshold
// in the HistogramOpts to create a zero bucket of width zero, i.e. a zero
// bucket that only receives observations of precisely zero.
const NativeHistogramZeroThresholdZero = -1

var errBucketLabelNotAllowed = fmt.Errorf(
	"%q is not allowed as label name in histograms", bucketLabel,
)

// LinearBuckets creates 'count' regular buckets, each 'width' wide, where the
// lowest bucket has an upper bound of 'start'. The final +Inf bucket is not
// counted and not included in the returned slice. The returned slice is meant
// to be used for the Buckets field of HistogramOpts.
//
// The function panics if 'count' is zero or negative.
func LinearBuckets(start, width float64, count int) []float64 {
	if count < 1 {
		panic("LinearBuckets needs a positive count")
	}
	buckets := make([]float64, count)
	for i := range buckets {
		buckets[i] = start
		start += width
	}
	return buckets
}

// ExponentialBuckets creates 'count' regular buckets, where the lowest bucket
// has an upper bound of 'start' and each following bucket's upper bound is
// 'factor' times the previous bucket's upper bound. The final +Inf bucket is
// not counted and not included in the returned slice. The returned slice is
// meant to be used for the Buckets field of HistogramOpts.
//
// The function panics if 'count' is 0 or negative, if 'start' is 0 or negative,
// or if 'factor' is less than or equal 1.
func ExponentialBuckets(start, factor float64, count int) []float64 {
	if count < 1 {
		panic("ExponentialBuckets needs a positive count")
	}
	if start <= 0 {
		panic("ExponentialBuckets needs a positive start value")
	}
	if factor <= 1 {
		panic("ExponentialBuckets needs a factor greater than 1")
	}
	buckets := make([]float64, count)
	for i := range buckets {
		buckets[i] = start
		start *= factor
	}
	return buckets
}

// ExponentialBucketsRange creates 'count' buckets, where the lowest bucket is
// 'min' and the highest bucket is 'max'. The final +Inf bucket is not counted
// and not included in the returned slice. The returned slice is meant to be
// used for the Buckets field of HistogramOpts.
//
// The function panics if 'count' is 0 or negative, if 'min' is 0 or negative.
func ExponentialBucketsRange(min, max float64, count int) []float64 {
	if count < 1 {
		panic("ExponentialBucketsRange count needs a positive count")
	}
	if min <= 0 {
		panic("ExponentialBucketsRange min needs to be greater than 0")
	}

	// Formula for exponential buckets.
	// max = min*growthFactor^(bucketCount-1)

	// We know max/min and highest bucket. Solve for growthFactor.
	growthFactor := math.Pow(max/min, 1.0/float64(count-1))

	// Now that we know growthFactor, solve for each bucket.
	buckets := make([]float64, count)
	for i := 1; i <= count; i++ {
		buckets[i-1] = min * math.Pow(growthFactor, float64(i-1))
	}
	return buckets
}

// HistogramOpts bundles the options for creating a Histogram metric. It is
// mandatory to set Name to a non-empty string. All other fields are optional
// and can safely be left at their zero value, although it is strongly
// encouraged to set a Help string.
type HistogramOpts struct {
	// Namespace, Subsystem, and Name are components of the fully-qualified
	// name of the Histogram (created by joining these components with
	// "_"). Only Name is mandatory, the others merely help structuring the
	// name. Note that the fully-qualified name of the Histogram must be a
	// valid Prometheus metric name.
	Namespace string
	Subsystem string
	Name      string

	// Help provides information about this Histogram.
	//
	// Metrics with the same fully-qualified name must have the same Help
	// string.
	Help string

	// ConstLabels are used to attach fixed labels to this metric. Metrics
	// with the same fully-qualified name must have the same label names in
	// their ConstLabels.
	//
	// ConstLabels are only used rarely. In particular, do not use them to
	// attach the same labels to all your metrics. Those use cases are
	// better covered by target labels set by the scraping Prometheus
	// server, or by one specific metric (e.g. a build_info or a
	// machine_role metric). See also
	// https://prometheus.io/docs/instrumenting/writing_exporters/#target-labels-not-static-scraped-labels
	ConstLabels Labels

	// Buckets defines the buckets into which observations are counted. Each
	// element in the slice is the upper inclusive bound of a bucket. The
	// values must be sorted in strictly increasing order. There is no need
	// to add a highest bucket with +Inf bound, it will be added
	// implicitly. If Buckets is left as nil or set to a slice of length
	// zero, it is replaced by default buckets. The default buckets are
	// DefBuckets if no buckets for a native histogram (see below) are used,
	// otherwise the default is no buckets. (In other words, if you want to
	// use both regular buckets and buckets for a native histogram, you have
	// to define the regular buckets here explicitly.)
	Buckets []float64

	// If NativeHistogramBucketFactor is greater than one, so-called sparse
	// buckets are used (in addition to the regular buckets, if defined
	// above). A Histogram with sparse buckets will be ingested as a Native
	// Histogram by a Prometheus server with that feature enabled (requires
	// Prometheus v2.40+). Sparse buckets are exponential buckets covering
	// the whole float64 range (with the exception of the “zero” bucket, see
	// NativeHistogramZeroThreshold below). From any one bucket to the next,
	// the width of the bucket grows by a constant
	// factor. NativeHistogramBucketFactor provides an upper bound for this
	// factor (exception see below). The smaller
	// NativeHistogramBucketFactor, the more buckets will be used and thus
	// the more costly the histogram will become. A generally good trade-off
	// between cost and accuracy is a value of 1.1 (each bucket is at most
	// 10% wider than the previous one), which will result in each power of
	// two divided into 8 buckets (e.g. there will be 8 buckets between 1
	// and 2, same as between 2 and 4, and 4 and 8, etc.).
	//
	// Details about the actually used factor: The factor is calculated as
	// 2^(2^-n), where n is an integer number between (and including) -4 and
	// 8. n is chosen so that the resulting factor is the largest that is
	// still smaller or equal to NativeHistogramBucketFactor. Note that the
	// smallest possible factor is therefore approx. 1.00271 (i.e. 2^(2^-8)
	// ). If NativeHistogramBucketFactor is greater than 1 but smaller than
	// 2^(2^-8), then the actually used factor is still 2^(2^-8) even though
	// it is larger than the provided NativeHistogramBucketFactor.
	//
	// NOTE: Native Histograms are still an experimental feature. Their
	// behavior might still change without a major version
	// bump. Subsequently, all NativeHistogram... options here might still
	// change their behavior or name (or might completely disappear) without
	// a major version bump.
	NativeHistogramBucketFactor float64
	// All observations with an absolute value of less or equal
	// NativeHistogramZeroThreshold are accumulated into a “zero” bucket.
	// For best results, this should be close to a bucket boundary. This is
	// usually the case if picking a power of two. If
	// NativeHistogramZeroThreshold is left at zero,
	// DefNativeHistogramZeroThreshold is used as the threshold. To
	// configure a zero bucket with an actual threshold of zero (i.e. only
	// observations of precisely zero will go into the zero bucket), set
	// NativeHistogramZeroThreshold to the NativeHistogramZeroThresholdZero
	// constant (or any negative float value).
	NativeHistogramZeroThreshold float64

	// The next three fields define a strategy to limit the number of
	// populated sparse buckets. If NativeHistogramMaxBucketNumber is left
	// at zero, the number of buckets is not limited. (Note that this might
	// lead to unbounded memory consumption if the values observed by the
	// Histogram are sufficiently wide-spread. In particular, this could be
	// used as a DoS attack vector. Where the observed values depend on
	// external inputs, it is highly recommended to set a
	// NativeHistogramMaxBucketNumber.) Once the set
	// NativeHistogramMaxBucketNumber is exceeded, the following strategy is
	// enacted:
	//  - First, if the last reset (or the creation) of the histogram is at
	//    least NativeHistogramMinResetDuration ago, then the whole
	//    histogram is reset to its initial state (including regular
	//    buckets).
	//  - If less time has passed, or if NativeHistogramMinResetDuration is
	//    zero, no reset is performed. Instead, the zero threshold is
	//    increased sufficiently to reduce the number of buckets to or below
	//    NativeHistogramMaxBucketNumber, but not to more than
	//    NativeHistogramMaxZeroThreshold. Thus, if
	//    NativeHistogramMaxZeroThreshold is already at or below the current
	//    zero threshold, nothing happens at this step.
	//  - After that, if the number of buckets still exceeds
	//    NativeHistogramMaxBucketNumber, the resolution of the histogram is
	//    reduced by doubling the width of the sparse buckets (up to a
	//    growth factor between one bucket to the next of 2^(2^4) = 65536,
	//    see above).
	//  - Any increased zero threshold or reduced resolution is reset back
	//    to their original values once NativeHistogramMinResetDuration has
	//    passed (since the last reset or the creation of the histogram).
	NativeHistogramMaxBucketNumber  uint32
	NativeHistogramMinResetDuration time.Duration
	NativeHistogramMaxZeroThreshold float64

	// NativeHistogramMaxExemplars limits the number of exemplars
	// that are kept in memory for each native histogram. If you leave it at
	// zero, a default value of 10 is used. If no exemplars should be kept specifically
	// for native histograms, set it to a negative value. (Scrapers can
	// still use the exemplars exposed for classic buckets, which are managed
	// independently.)
	NativeHistogramMaxExemplars int
	// NativeHistogramExemplarTTL is only checked once
	// NativeHistogramMaxExemplars is exceeded. In that case, the
	// oldest exemplar is removed if it is older than NativeHistogramExemplarTTL.
	// Otherwise, the older exemplar in the pair of exemplars that are closest
	// together (on an exponential scale) is removed.
	// If NativeHistogramExemplarTTL is left at its zero value, a default value of
	// 5m is used. To always delete the oldest exemplar, set it to a negative value.
	NativeHistogramExemplarTTL time.Duration

	// now is for testing purposes, by default it's time.Now.
	now func() time.Time

	// afterFunc is for testing purposes, by default it's time.AfterFunc.
	afterFunc func(time.Duration, func()) *time.Timer
}

// HistogramVecOpts bundles the options to create a HistogramVec metric.
// It is mandatory to set HistogramOpts, see there for mandatory fields. VariableLabels
// is optional and can safely be left to its default value.
type HistogramVecOpts struct {
	HistogramOpts

	// VariableLabels are used to partition the metric vector by the given set
	// of labels. Each label value will be constrained with the optional Constraint
	// function, if provided.
	VariableLabels ConstrainableLabels
}

// NewHistogram creates a new Histogram based on the provided HistogramOpts. It
// panics if the buckets in HistogramOpts are not in strictly increasing order.
//
// The returned implementation also implements ExemplarObserver. It is safe to
// perform the corresponding type assertion. Exemplars are tracked separately
// for each bucket.
func NewHistogram(opts HistogramOpts) Histogram {
	return newHistogram(
		NewDesc(
			BuildFQName(opts.Namespace, opts.Subsystem, opts.Name),
			opts.Help,
			nil,
			opts.ConstLabels,
		),
		opts,
	)
}

func newHistogram(desc *Desc, opts HistogramOpts, labelValues ...string) Histogram {
	if len(desc.variableLabels.names) != len(labelValues) {
		panic(makeInconsistentCardinalityError(desc.fqName, desc.variableLabels.names, labelValues))
	}

	for _, n := range desc.variableLabels.names {
		if n == bucketLabel {
			panic(errBucketLabelNotAllowed)
		}
	}
	for _, lp := range desc.constLabelPairs {
		if lp.GetName() == bucketLabel {
			panic(errBucketLabelNotAllowed)
		}
	}

	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.afterFunc == nil {
		opts.afterFunc = time.AfterFunc
	}

	h := &histogram{
		desc:                            desc,
		upperBounds:                     opts.Buckets,
		labelPairs:                      MakeLabelPairs(desc, labelValues),
		nativeHistogramMaxBuckets:       opts.NativeHistogramMaxBucketNumber,
		nativeHistogramMaxZeroThreshold: opts.NativeHistogramMaxZeroThreshold,
		nativeHistogramMinResetDuration: opts.NativeHistogramMinResetDuration,
		lastResetTime:                   opts.now(),
		now:                             opts.now,
		afterFunc:                       opts.afterFunc,
	}
	if len(h.upperBounds) == 0 && opts.NativeHistogramBucketFactor <= 1 {
		h.upperBounds = DefBuckets
	}
	if opts.NativeHistogramBucketFactor <= 1 {
		h.nativeHistogramSchema = math.MinInt32 // To mark that there are no sparse buckets.
	} else {
		switch {
		case opts.NativeHistogramZeroThreshold > 0:
			h.nativeHistogramZeroThreshold = opts.NativeHistogramZeroThreshold
		case opts.NativeHistogramZeroThreshold == 0:
			h.nativeHistogramZeroThreshold = DefNativeHistogramZeroThreshold
		} // Leave h.nativeHistogramZeroThreshold at 0 otherwise.
		h.nativeHistogramSchema = pickSchema(opts.NativeHistogramBucketFactor)
		h.nativeExemplars = makeNativeExemplars(opts.NativeHistogramExemplarTTL, opts.NativeHistogramMaxExemplars)
	}
	for i, upperBound := range h.upperBounds {
		if i < len(h.upperBounds)-1 {
			if upperBound >= h.upperBounds[i+1] {
				panic(fmt.Errorf(
					"histogram buckets must be in increasing order: %f >= %f",
					upperBound, h.upperBounds[i+1],
				))
			}
		} else {
			if math.IsInf(upperBound, +1) {
				// The +Inf bucket is implicit. Remove it here.
				h.upperBounds = h.upperBounds[:i]
			}
		}
	}
	// Finally we know the final length of h.upperBounds and can make buckets
	// for both counts as well as exemplars:
	h.counts[0] = &histogramCounts{buckets: make([]uint64, len(h.upperBounds))}
	atomic.StoreUint64(&h.counts[0].nativeHistogramZeroThresholdBits, math.Float64bits(h.nativeHistogramZeroThreshold))
	atomic.StoreInt32(&h.counts[0].nativeHistogramSchema, h.nativeHistogramSchema)
	h.counts[1] = &histogramCounts{buckets: make([]uint64, len(h.upperBounds))}
	atomic.StoreUint64(&h.counts[1].nativeHistogramZeroThresholdBits, math.Float64bits(h.nativeHistogramZeroThreshold))
	atomic.StoreInt32(&h.counts[1].nativeHistogramSchema, h.nativeHistogramSchema)
	h.exemplars = make([]atomic.Value, len(h.upperBounds)+1)

	h.init(h) // Init self-collection.
	return h
}

type histogramCounts struct {
	// Order in this struct matters for the alignment required by atomic
	// operations, see http://golang.org/pkg/sync/atomic/#pkg-note-BUG

	// sumBits contains the bits of the float64 representing the sum of all
	// observations.
	sumBits uint64
	count   uint64

	// nativeHistogramZeroBucket counts all (positive and negative)
	// observations in the zero bucket (with an absolute value less or equal
	// the current threshold, see next field.
	nativeHistogramZeroBucket uint64
	// nativeHistogramZeroThresholdBits is the bit pattern of the current
	// threshold for the zero bucket. It's initially equal to
	// nativeHistogramZeroThreshold but may change according to the bucket
	// count limitation strategy.
	nativeHistogramZeroThresholdBits uint64
	// nativeHistogramSchema may change over time according to the bucket
	// count limitation strategy and therefore has to be saved here.
	nativeHistogramSchema int32
	// Number of (positive and negative) sparse buckets.
	nativeHistogramBucketsNumber uint32

	// Regular buckets.
	buckets []uint64

	// The sparse buckets for native histograms are implemented with a
	// sync.Map for now. A dedicated data structure will likely be more
	// efficient. There are separate maps for negative and positive
	// observations. The map's value is an *int64, counting observations in
	// that bucket. (Note that we don't use uint64 as an int64 won't
	// overflow in practice, and working with signed numbers from the
	// beginning simplifies the handling of deltas.) The map's key is the
	// index of the bucket according to the used
	// nativeHistogramSchema. Index 0 is for an upper bound of 1.
	nativeHistogramBucketsPositive, nativeHistogramBucketsNegative sync.Map
}

// observe manages the parts of observe that only affects
// histogramCounts. doSparse is true if sparse buckets should be done,
// too.
func (hc *histogramCounts) observe(v float64, bucket int, doSparse bool) {
	if bucket < len(hc.buckets) {
		atomic.AddUint64(&hc.buckets[bucket], 1)
	}
	atomicAddFloat(&hc.sumBits, v)
	if doSparse && !math.IsNaN(v) {
		var (
			key                  int
			schema               = atomic.LoadInt32(&hc.nativeHistogramSchema)
			zeroThreshold        = math.Float64frombits(atomic.LoadUint64(&hc.nativeHistogramZeroThresholdBits))
			bucketCreated, isInf bool
		)
		if math.IsInf(v, 0) {
			// Pretend v is MaxFloat64 but later increment key by one.
			if math.IsInf(v, +1) {
				v = math.MaxFloat64
			} else {
				v = -math.MaxFloat64
			}
			isInf = true
		}
		frac, exp := math.Frexp(math.Abs(v))
		if schema > 0 {
			bounds := nativeHistogramBounds[schema]
			key = sort.SearchFloat64s(bounds, frac) + (exp-1)*len(bounds)
		} else {
			key = exp
			if frac == 0.5 {
				key--
			}
			offset := (1 << -schema) - 1
			key = (key + offset) >> -schema
		}
		if isInf {
			key++
		}
		switch {
		case v > zeroThreshold:
			bucketCreated = addToBucket(&hc.nativeHistogramBucketsPositive, key, 1)
		case v < -zeroThreshold:
			bucketCreated = addToBucket(&hc.nativeHistogramBucketsNegative, key, 1)
		default:
			atomic.AddUint64(&hc.nativeHistogramZeroBucket, 1)
		}
		if bucketCreated {
			atomic.AddUint32(&hc.nativeHistogramBucketsNumber, 1)
		}
	}
	// Increment count last as we take it as a signal that the observation
	// is complete.
	atomic.AddUint64(&hc.count, 1)
}

type histogram struct {
	// countAndHotIdx enables lock-free writes with use of atomic updates.
	// The most significant bit is the hot index [0 or 1] of the count field
	// below. Observe calls update the hot one. All remaining bits count the
	// number of Observe calls. Observe starts by incrementing this counter,
	// and finish by incrementing the count field in the respective
	// histogramCounts, as a marker for completion.
	//
	// Calls of the Write method (which are non-mutating reads from the
	// perspective of the histogram) swap the hot–cold under the writeMtx
	// lock. A cooldown is awaited (while locked) by comparing the number of
	// observations with the initiation count. Once they match, then the
	// last observation on the now cool one has completed. All cold fields must
	// be merged into the new hot before releasing writeMtx.
	//
	// Fields with atomic access first! See alignment constraint:
	// http://golang.org/pkg/sync/atomic/#pkg-note-BUG
	countAndHotIdx uint64

	selfCollector
	desc *Desc

	// Only used in the Write method and for sparse bucket management.
	mtx sync.Mutex

	// Two counts, one is "hot" for lock-free observations, the other is
	// "cold" for writing out a dto.Metric. It has to be an array of
	// pointers to guarantee 64bit alignment of the histogramCounts, see
	// http://golang.org/pkg/sync/atomic/#pkg-note-BUG.
	counts [2]*histogramCounts

	upperBounds                     []float64
	labelPairs                      []*dto.LabelPair
	exemplars                       []atomic.Value // One more than buckets (to include +Inf), each a *dto.Exemplar.
	nativeHistogramSchema           int32          // The initial schema. Set to math.MinInt32 if no sparse buckets are used.
	nativeHistogramZeroThreshold    float64        // The initial zero threshold.
	nativeHistogramMaxZeroThreshold float64
	nativeHistogramMaxBuckets       uint32
	nativeHistogramMinResetDuration time.Duration
	// lastResetTime is protected by mtx. It is also used as created timestamp.
	lastResetTime time.Time
	// resetScheduled is protected by mtx. It is true if a reset is
	// scheduled for a later time (when nativeHistogramMinResetDuration has
	// passed).
	resetScheduled  bool
	nativeExemplars nativeExemplars

	// now is for testing purposes, by default it's time.Now.
	now func() time.Time

	// afterFunc is for testing purposes, by default it's time.AfterFunc.
	afterFunc func(time.Duration, func()) *time.Timer
}

func (h *histogram) Desc() *Desc {
	return h.desc
}

func (h *histogram) Observe(v float64) {
	h.observe(v, h.findBucket(v))
}

// ObserveWithExemplar should not be called in a high-frequency setting
// for a native histogram with configured exemplars. For this case,
// the implementation isn't lock-free and might suffer from lock contention.
func (h *histogram) ObserveWithExemplar(v float64, e Labels) {
	i := h.findBucket(v)
	h.observe(v, i)
	h.updateExemplar(v, i, e)
}

func (h *histogram) Write(out *dto.Metric) error {
	// For simplicity, we protect this whole method by a mutex. It is not in
	// the hot path, i.e. Observe is called much more often than Write. The
	// complication of making Write lock-free isn't worth it, if possible at
	// all.
	h.mtx.Lock()
	defer h.mtx.Unlock()

	// Adding 1<<63 switches the hot index (from 0 to 1 or from 1 to 0)
	// without touching the count bits. See the struct comments for a full
	// description of the algorithm.
	n := atomic.AddUint64(&h.countAndHotIdx, 1<<63)
	// count is contained unchanged in the lower 63 bits.
	count := n & ((1 << 63) - 1)
	// The most significant bit tells us which counts is hot. The complement
	// is thus the cold one.
	hotCounts := h.counts[n>>63]
	coldCounts := h.counts[(^n)>>63]

	waitForCooldown(count, coldCounts)

	his := &dto.Histogram{
		Bucket:           make([]*dto.Bucket, len(h.upperBounds)),
		SampleCount:      proto.Uint64(count),
		SampleSum:        proto.Float64(math.Float64frombits(atomic.LoadUint64(&coldCounts.sumBits))),
		CreatedTimestamp: timestamppb.New(h.lastResetTime),
	}
	out.Histogram = his
	out.Label = h.labelPairs

	var cumCount uint64
	for i, upperBound := range h.upperBounds {
		cumCount += atomic.LoadUint64(&coldCounts.buckets[i])
		his.Bucket[i] = &dto.Bucket{
			CumulativeCount: proto.Uint64(cumCount),
			UpperBound:      proto.Float64(upperBound),
		}
		if e := h.exemplars[i].Load(); e != nil {
			his.Bucket[i].Exemplar = e.(*dto.Exemplar)
		}
	}
	// If there is an exemplar for the +Inf bucket, we have to add that bucket explicitly.
	if e := h.exemplars[len(h.upperBounds)].Load(); e != nil {
		b := &dto.Bucket{
			CumulativeCount: proto.Uint64(count),
			UpperBound:      proto.Float64(math.Inf(1)),
			Exemplar:        e.(*dto.Exemplar),
		}
		his.Bucket = append(his.Bucket, b)
	}
	if h.nativeHistogramSchema > math.MinInt32 {
		his.ZeroThreshold = proto.Float64(math.Float64frombits(atomic.LoadUint64(&coldCounts.nativeHistogramZeroThresholdBits)))
		his.Schema = proto.Int32(atomic.LoadInt32(&coldCounts.nativeHistogramSchema))
		zeroBucket := atomic.LoadUint64(&coldCounts.nativeHistogramZeroBucket)

		defer func() {
			coldCounts.nativeHistogramBucketsPositive.Range(addAndReset(&hotCounts.nativeHistogramBucketsPositive, &hotCounts.nativeHistogramBucketsNumber))
			coldCounts.nativeHistogramBucketsNegative.Range(addAndReset(&hotCounts.nativeHistogramBucketsNegative, &hotCounts.nativeHistogramBucketsNumber))
		}()

		his.ZeroCount = proto.Uint64(zeroBucket)
		his.NegativeSpan, his.NegativeDelta = makeBuckets(&coldCounts.nativeHistogramBucketsNegative)
		his.PositiveSpan, his.PositiveDelta = makeBuckets(&coldCounts.nativeHistogramBucketsPositive)

		// Add a no-op span to a histogram without observations and with
		// a zero threshold of zero. Otherwise, a native histogram would
		// look like a classic histogram to scrapers.
		if *his.ZeroThreshold == 0 && *his.ZeroCount == 0 && len(his.PositiveSpan) == 0 && len(his.NegativeSpan) == 0 {
			his.PositiveSpan = []*dto.BucketSpan{{
				Offset: proto.Int32(0),
				Length: proto.Uint32(0),
			}}
		}

		if h.nativeExemplars.isEnabled() {
			h.nativeExemplars.Lock()
			his.Exemplars = append(his.Exemplars, h.nativeExemplars.exemplars...)
			h.nativeExemplars.Unlock()
		}

	}
	addAndResetCounts(hotCounts, coldCounts)
	return nil
}

// findBucket returns the index of the bucket for the provided value, or
// len(h.upperBounds) for the +Inf bucket.
func (h *histogram) findBucket(v float64) int {
	// TODO(beorn7): For small numbers of buckets (<30), a linear search is
	// slightly faster than the binary search. If we really care, we could
	// switch from one search strategy to the other depending on the number
	// of buckets.
	//
	// Microbenchmarks (BenchmarkHistogramNoLabels):
	// 11 buckets: 38.3 ns/op linear - binary 48.7 ns/op
	// 100 buckets: 78.1 ns/op linear - binary 54.9 ns/op
	// 300 buckets: 154 ns/op linear - binary 61.6 ns/op
	return sort.SearchFloat64s(h.upperBounds, v)
}

// observe is the implementation for Observe without the findBucket part.
func (h *histogram) observe(v float64, bucket int) {
	// Do not add to sparse buckets for NaN observations.
	doSparse := h.nativeHistogramSchema > math.MinInt32 && !math.IsNaN(v)
	// We increment h.countAndHotIdx so that the counter in the lower
	// 63 bits gets incremented. At the same time, we get the new value
	// back, which we can use to find the currently-hot counts.
	n := atomic.AddUint64(&h.countAndHotIdx, 1)
	hotCounts := h.counts[n>>63]
	hotCounts.observe(v, bucket, doSparse)
	if doSparse {
		h.limitBuckets(hotCounts, v, bucket)
	}
}

// limitBuckets applies a strategy to limit the number of populated sparse
// buckets. It's generally best effort, and there are situations where the
// number can go higher (if even the lowest resolution isn't enough to reduce
// the number sufficiently, or if the provided counts aren't fully updated yet
// by a concurrently happening Write call).
func (h *histogram) limitBuckets(counts *histogramCounts, value float64, bucket int) {
	if h.nativeHistogramMaxBuckets == 0 {
		return // No limit configured.
	}
	if h.nativeHistogramMaxBuckets >= atomic.LoadUint32(&counts.nativeHistogramBucketsNumber) {
		return // Bucket limit not exceeded yet.
	}

	h.mtx.Lock()
	defer h.mtx.Unlock()

	// The hot counts might have been swapped just before we acquired the
	// lock. Re-fetch the hot counts first...
	n := atomic.LoadUint64(&h.countAndHotIdx)
	hotIdx := n >> 63
	coldIdx := (^n) >> 63
	hotCounts := h.counts[hotIdx]
	coldCounts := h.counts[coldIdx]
	// ...and then check again if we really have to reduce the bucket count.
	if h.nativeHistogramMaxBuckets >= atomic.LoadUint32(&hotCounts.nativeHistogramBucketsNumber) {
		return // Bucket limit not exceeded after all.
	}
	// Try the various strategies in order.
	if h.maybeReset(hotCounts, coldCounts, coldIdx, value, bucket) {
		return
	}
	// One of the other strategies will happen. To undo what they will do as
	// soon as enough time has passed to satisfy
	// h.nativeHistogramMinResetDuration, schedule a reset at the right time
	// if we haven't done so already.
	if h.nativeHistogramMinResetDuration > 0 && !h.resetScheduled {
		h.resetScheduled = true
		h.afterFunc(h.nativeHistogramMinResetDuration-h.now().Sub(h.lastResetTime), h.reset)
	}

	if h.maybeWidenZeroBucket(hotCounts, coldCounts) {
		return
	}
	h.doubleBucketWidth(hotCounts, coldCounts)
}

// maybeReset resets the whole histogram if at least
// h.nativeHistogramMinResetDuration has been passed. It returns true if the
// histogram has been reset. The caller must have locked h.mtx.
func (h *histogram) maybeReset(
	hot, cold *histogramCounts, coldIdx uint64, value float64, bucket int,
) bool {
	// We are using the possibly mocked h.now() rather than
	// time.Since(h.lastResetTime) to enable testing.
	if h.nativeHistogramMinResetDuration == 0 || // No reset configured.
		h.resetScheduled || // Do not interefere if a reset is already scheduled.
		h.now().Sub(h.lastResetTime) < h.nativeHistogramMinResetDuration {
		return false
	}
	// Completely reset coldCounts.
	h.resetCounts(cold)
	// Repeat the latest observation to not lose it completely.
	cold.observe(value, bucket, true)
	// Make coldCounts the new hot counts while resetting countAndHotIdx.
	n := atomic.SwapUint64(&h.countAndHotIdx, (coldIdx<<63)+1)
	count := n & ((1 << 63) - 1)
	waitForCooldown(count, hot)
	// Finally, reset the formerly hot counts, too.
	h.resetCounts(hot)
	h.lastResetTime = h.now()
	return true
}

// reset resets the whole histogram. It locks h.mtx itself, i.e. it has to be
// called without having locked h.mtx.
func (h *histogram) reset() {
	h.mtx.Lock()
	defer h.mtx.Unlock()

	n := atomic.LoadUint64(&h.countAndHotIdx)
	hotIdx := n >> 63
	coldIdx := (^n) >> 63
	hot := h.counts[hotIdx]
	cold := h.counts[coldIdx]
	// Completely reset coldCounts.
	h.resetCounts(cold)
	// Make coldCounts the new hot counts while resetting countAndHotIdx.
	n = atomic.SwapUint64(&h.countAndHotIdx, coldIdx<<63)
	count := n & ((1 << 63) - 1)
	waitForCooldown(count, hot)
	// Finally, reset the formerly hot counts, too.
	h.resetCounts(hot)
	h.lastResetTime = h.now()
	h.resetScheduled = false
}

// maybeWidenZeroBucket widens the zero bucket until it includes the existing
// buckets closest to the zero bucket (which could be two, if an equidistant
// negative and a positive bucket exists, but usually it's only one bucket to be
// merged into the new wider zero bucket). h.nativeHistogramMaxZeroThreshold
// limits how far the zero bucket can be extended, and if that's not enough to
// include an existing bucket, the method returns false. The caller must have
// locked h.mtx.
func (h *histogram) maybeWidenZeroBucket(hot, cold *histogramCounts) bool {
	currentZeroThreshold := math.Float64frombits(atomic.LoadUint64(&hot.nativeHistogramZeroThresholdBits))
	if currentZeroThreshold >= h.nativeHistogramMaxZeroThreshold {
		return false
	}
	// Find the key of the bucket closest to zero.
	smallestKey := findSmallestKey(&hot.nativeHistogramBucketsPositive)
	smallestNegativeKey := findSmallestKey(&hot.nativeHistogramBucketsNegative)
	if smallestNegativeKey < smallestKey {
		smallestKey = smallestNegativeKey
	}
	if smallestKey == math.MaxInt32 {
		return false
	}
	newZeroThreshold := getLe(smallestKey, atomic.LoadInt32(&hot.nativeHistogramSchema))
	if newZeroThreshold > h.nativeHistogramMaxZeroThreshold {
		return false // New threshold would exceed the max threshold.
	}
	atomic.StoreUint64(&cold.nativeHistogramZeroThresholdBits, math.Float64bits(newZeroThreshold))
	// Remove applicable buckets.
	if _, loaded := cold.nativeHistogramBucketsNegative.LoadAndDelete(smallestKey); loaded {
		atomicDecUint32(&cold.nativeHistogramBucketsNumber)
	}
	if _, loaded := cold.nativeHistogramBucketsPositive.LoadAndDelete(smallestKey); loaded {
		atomicDecUint32(&cold.nativeHistogramBucketsNumber)
	}
	// Make cold counts the new hot counts.
	n := atomic.AddUint64(&h.countAndHotIdx, 1<<63)
	count := n & ((1 << 63) - 1)
	// Swap the pointer names to represent the new roles and make
	// the rest less confusing.
	hot, cold = cold, hot
	waitForCooldown(count, cold)
	// Add all the now cold counts to the new hot counts...
	addAndResetCounts(hot, cold)
	// ...adjust the new zero threshold in the cold counts, too...
	atomic.StoreUint64(&cold.nativeHistogramZeroThresholdBits, math.Float64bits(newZeroThreshold))
	// ...and then merge the newly deleted buckets into the wider zero
	// bucket.
	mergeAndDeleteOrAddAndReset := func(hotBuckets, coldBuckets *sync.Map) func(k, v interface{}) bool {
		return func(k, v interface{}) bool {
			key := k.(int)
			bucket := v.(*int64)
			if key == smallestKey {
				// Merge into hot zero bucket...
				atomic.AddUint64(&hot.nativeHistogramZeroBucket, uint64(atomic.LoadInt64(bucket)))
				// ...and delete from cold counts.
				coldBuckets.Delete(key)
				atomicDecUint32(&cold.nativeHistogramBucketsNumber)
			} else {
				// Add to corresponding hot bucket...
				if addToBucket(hotBuckets, key, atomic.LoadInt64(bucket)) {
					atomic.AddUint32(&hot.nativeHistogramBucketsNumber, 1)
				}
				// ...and reset cold bucket.
				atomic.StoreInt64(bucket, 0)
			}
			return true
		}
	}

	cold.nativeHistogramBucketsPositive.Range(mergeAndDeleteOrAddAndReset(&hot.nativeHistogramBucketsPositive, &cold.nativeHistogramBucketsPositive))
	cold.nativeHistogramBucketsNegative.Range(mergeAndDeleteOrAddAndReset(&hot.nativeHistogramBucketsNegative, &cold.nativeHistogramBucketsNegative))
	return true
}

// doubleBucketWidth doubles the bucket width (by decrementing the schema
// number). Note that very sparse buckets could lead to a low reduction of the
// bucket count (or even no reduction at all). The method does nothing if the
// schema is already -4.
func (h *histogram) doubleBucketWidth(hot, cold *histogramCounts) {
	coldSchema := atomic.LoadInt32(&cold.nativeHistogramSchema)
	if coldSchema == -4 {
		return // Already at lowest resolution.
	}
	coldSchema--
	atomic.StoreInt32(&cold.nativeHistogramSchema, coldSchema)
	// Play it simple and just delete all cold buckets.
	atomic.StoreUint32(&cold.nativeHistogramBucketsNumber, 0)
	deleteSyncMap(&cold.nativeHistogramBucketsNegative)
	deleteSyncMap(&cold.nativeHistogramBucketsPositive)
	// Make coldCounts the new hot counts.
	n := atomic.AddUint64(&h.countAndHotIdx, 1<<63)
	count := n & ((1 << 63) - 1)
	// Swap the pointer names to represent the new roles and make
	// the rest less confusing.
	hot, cold = cold, hot
	waitForCooldown(count, cold)
	// Add all the now cold counts to the new hot counts...
	addAndResetCounts(hot, cold)
	// ...adjust the schema in the cold counts, too...
	atomic.StoreInt32(&cold.nativeHistogramSchema, coldSchema)
	// ...and then merge the cold buckets into the wider hot buckets.
	merge := func(hotBuckets *sync.Map) func(k, v interface{}) bool {
		return func(k, v interface{}) bool {
			key := k.(int)
			bucket := v.(*int64)
			// Adjust key to match the bucket to merge into.
			if key > 0 {
				key++
			}
			key /= 2
			// Add to corresponding hot bucket.
			if addToBucket(hotBuckets, key, atomic.LoadInt64(bucket)) {
				atomic.AddUint32(&hot.nativeHistogramBucketsNumber, 1)
			}
			return true
		}
	}

	cold.nativeHistogramBucketsPositive.Range(merge(&hot.nativeHistogramBucketsPositive))
	cold.nativeHistogramBucketsNegative.Range(merge(&hot.nativeHistogramBucketsNegative))
	// Play it simple again and just delete all cold buckets.
	atomic.StoreUint32(&cold.nativeHistogramBucketsNumber, 0)
	deleteSyncMap(&cold.nativeHistogramBucketsNegative)
	deleteSyncMap(&cold.nativeHistogramBucketsPositive)
}

func (h *histogram) resetCounts(counts *histogramCounts) {
	atomic.StoreUint64(&counts.sumBits, 0)
	atomic.StoreUint64(&counts.count, 0)
	atomic.StoreUint64(&counts.nativeHistogramZeroBucket, 0)
	atomic.StoreUint64(&counts.nativeHistogramZeroThresholdBits, math.Float64bits(h.nativeHistogramZeroThreshold))
	atomic.StoreInt32(&counts.nativeHistogramSchema, h.nativeHistogramSchema)
	atomic.StoreUint32(&counts.nativeHistogramBucketsNumber, 0)
	for i := range h.upperBounds {
		atomic.StoreUint64(&counts.buckets[i], 0)
	}
	deleteSyncMap(&counts.nativeHistogramBucketsNegative)
	deleteSyncMap(&counts.nativeHistogramBucketsPositive)
}

// updateExemplar replaces the exemplar for the provided classic bucket.
// With empty labels, it's a no-op. It panics if any of the labels is invalid.
// If histogram is native, the exemplar will be cached into nativeExemplars,
// which has a limit, and will remove one exemplar when limit is reached.
func (h *histogram) updateExemplar(v float64, bucket int, l Labels) {
	if l == nil {
		return
	}
	e, err := newExemplar(v, h.now(), l)
	if err != nil {
		panic(err)
	}
	h.exemplars[bucket].Store(e)
	doSparse := h.nativeHistogramSchema > math.MinInt32 && !math.IsNaN(v)
	if doSparse {
		h.nativeExemplars.addExemplar(e)
	}
}

// HistogramVec is a Collector that bundles a set of Histograms that all share the
// same Desc, but have different values for their variable labels. This is used
// if you want to count the same thing partitioned by various dimensions
// (e.g. HTTP request latencies, partitioned by status code and method). Create
// instances with NewHistogramVec.
type HistogramVec struct {
	*MetricVec
}

// NewHistogramVec creates a new HistogramVec based on the provided HistogramOpts and
// partitioned by the given label names.
func NewHistogramVec(opts HistogramOpts, labelNames []string) *HistogramVec {
	return V2.NewHistogramVec(HistogramVecOpts{
		HistogramOpts:  opts,
		VariableLabels: UnconstrainedLabels(labelNames),
	})
}

// NewHistogramVec creates a new HistogramVec based on the provided HistogramVecOpts.
func (v2) NewHistogramVec(opts HistogramVecOpts) *HistogramVec {
	desc := V2.NewDesc(
		BuildFQName(opts.Namespace, opts.Subsystem, opts.Name),
		opts.Help,
		opts.VariableLabels,
		opts.ConstLabels,
	)
	return &HistogramVec{
		MetricVec: NewMetricVec(desc, func(lvs ...string) Metric {
			return newHistogram(desc, opts.HistogramOpts, lvs...)
		}),
	}
}

// GetMetricWithLabelValues returns the Histogram for the given slice of label
// values (same order as the variable labels in Desc). If that combination of
// label values is accessed for the first time, a new Histogram is created.
//
// It is possible to call this method without using the returned Histogram to only
// create the new Histogram but leave it at its starting value, a Histogram without
// any observations.
//
// Keeping the Histogram for later use is possible (and should be considered if
// performance is critical), but keep in mind that Reset, DeleteLabelValues and
// Delete can be used to delete the Histogram from the HistogramVec. In that case, the
// Histogram will still exist, but it will not be exported anymore, even if a
// Histogram with the same label values is created later. See also the CounterVec
// example.
//
// An error is returned if the number of label values is not the same as the
// number of variable labels in Desc (minus any curried labels).
//
// Note that for more than one label value, this method is prone to mistakes
// caused by an incorrect order of arguments. Consider GetMetricWith(Labels) as
// an alternative to avoid that type of mistake. For higher label numbers, the
// latter has a much more readable (albeit more verbose) syntax, but it comes
// with a performance overhead (for creating and processing the Labels map).
// See also the GaugeVec example.
func (v *HistogramVec) GetMetricWithLabelValues(lvs ...string) (Observer, error) {
	metric, err := v.MetricVec.GetMetricWithLabelValues(lvs...)
	if metric != nil {
		return metric.(Observer), err
	}
	return nil, err
}

// GetMetricWith returns the Histogram for the given Labels map (the label names
// must match those of the variable labels in Desc). If that label map is
// accessed for the first time, a new Histogram is created. Implications of
// creating a Histogram without using it and keeping the Histogram for later use
// are the same as for GetMetricWithLabelValues.
//
// An error is returned if the number and names of the Labels are inconsistent
// with those of the variable labels in Desc (minus any curried labels).
//
// This method is used for the same purpose as
// GetMetricWithLabelValues(...string). See there for pros and cons of the two
// methods.
func (v *HistogramVec) GetMetricWith(labels Labels) (Observer, error) {
	metric, err := v.MetricVec.GetMetricWith(labels)
	if metric != nil {
		return metric.(Observer), err
	}
	return nil, err
}

// WithLabelValues works as GetMetricWithLabelValues, but panics where
// GetMetricWithLabelValues would have returned an error. Not returning an
// error allows shortcuts like
//
//	myVec.WithLabelValues("404", "GET").Observe(42.21)
func (v *HistogramVec) WithLabelValues(lvs ...string) Observer {
	h, err := v.GetMetricWithLabelValues(lvs...)
	if err != nil {
		panic(err)
	}
	return h
}

// With works as GetMetricWith but panics where GetMetricWithLabels would have
// returned an error. Not returning an error allows shortcuts like
//
//	myVec.With(prometheus.Labels{"code": "404", "method": "GET"}).Observe(42.21)
func (v *HistogramVec) With(labels Labels) Observer {
	h, err := v.GetMetricWith(labels)
	if err != nil {
		panic(err)
	}
	return h
}

// CurryWith returns a vector curried with the provided labels, i.e. the
// returned vector has those labels pre-set for all labeled operations performed
// on it. The cardinality of the curried vector is reduced accordingly. The
// order of the remaining labels stays the same (just with the curried labels
// taken out of the sequence – which is relevant for the
// (GetMetric)WithLabelValues methods). It is possible to curry a curried
// vector, but only with labels not yet used for currying before.
//
// The metrics contained in the HistogramVec are shared between the curried and
// uncurried vectors. They are just accessed differently. Curried and uncurried
// vectors behave identically in terms of collection. Only one must be
// registered with a given registry (usually the uncurried version). The Reset
// method deletes all metrics, even if called on a curried vector.
func (v *HistogramVec) CurryWith(labels Labels) (ObserverVec, error) {
	vec, err := v.MetricVec.CurryWith(labels)
	if vec != nil {
		return &HistogramVec{vec}, err
	}
	return nil, err
}

// MustCurryWith works as CurryWith but panics where CurryWith would have
// returned an error.
func (v *HistogramVec) MustCurryWith(labels Labels) ObserverVec {
	vec, err := v.CurryWith(labels)
	if err != nil {
		panic(err)
	}
	return vec
}

type constHistogram struct {
	desc       *Desc
	count      uint64
	sum        float64
	buckets    map[float64]uint64
	labelPairs []*dto.LabelPair
	createdTs  *timestamppb.Timestamp
}

func (h *constHistogram) Desc() *Desc {
	return h.desc
}

func (h *constHistogram) Write(out *dto.Metric) error {
	his := &dto.Histogram{
		CreatedTimestamp: h.createdTs,
	}

	buckets := make([]*dto.Bucket, 0, len(h.buckets))

	his.SampleCount = proto.Uint64(h.count)
	his.SampleSum = proto.Float64(h.sum)
	for upperBound, count := range h.buckets {
		buckets = append(buckets, &dto.Bucket{
			CumulativeCount: proto.Uint64(count),
			UpperBound:      proto.Float64(upperBound),
		})
	}

	if len(buckets) > 0 {
		sort.Sort(buckSort(buckets))
	}
	his.Bucket = buckets

	out.Histogram = his
	out.Label = h.labelPairs

	return nil
}

// NewConstHistogram returns a metric representing a Prometheus histogram with
// fixed values for the count, sum, and bucket counts. As those parameters
// cannot be changed, the returned value does not implement the Histogram
// interface (but only the Metric interface). Users of this package will not
// have much use for it in regular operations. However, when implementing custom
// Collectors, it is useful as a throw-away metric that is generated on the fly
// to send it to Prometheus in the Collect method.
//
// buckets is a map of upper bounds to cumulative counts, excluding the +Inf
// bucket. The +Inf bucket is implicit, and its value is equal to the provided count.
//
// NewConstHistogram returns an error if the length of labelValues is not
// consistent with the variable labels in Desc or if Desc is invalid.
func NewConstHistogram(
	desc *Desc,
	count uint64,
	sum float64,
	buckets map[float64]uint64,
	labelValues ...string,
) (Metric, error) {
	if desc.err != nil {
		return nil, desc.err
	}
	if err := validateLabelValues(labelValues, len(desc.variableLabels.names)); err != nil {
		return nil, err
	}
	return &constHistogram{
		desc:       desc,
		count:      count,
		sum:        sum,
		buckets:    buckets,
		labelPairs: MakeLabelPairs(desc, labelValues),
	}, nil
}

// MustNewConstHistogram is a version of NewConstHistogram that panics where
// NewConstHistogram would have returned an error.
func MustNewConstHistogram(
	desc *Desc,
	count uint64,
	sum float64,
	buckets map[float64]uint64,
	labelValues ...string,
) Metric {
	m, err := NewConstHistogram(desc, count, sum, buckets, labelValues...)
	if err != nil {
		panic(err)
	}
	return m
}

// NewConstHistogramWithCreatedTimestamp does the same thing as NewConstHistogram but sets the created timestamp.
func NewConstHistogramWithCreatedTimestamp(
	desc *Desc,
	count uint64,
	sum float64,
	buckets map[float64]uint64,
	ct time.Time,
	labelValues ...string,
) (Metric, error) {
	if desc.err != nil {
		return nil, desc.err
	}
	if err := validateLabelValues(labelValues, len(desc.variableLabels.names)); err != nil {
		return nil, err
	}
	return &constHistogram{
		desc:       desc,
		count:      count,
		sum:        sum,
		buckets:    buckets,
		labelPairs: MakeLabelPairs(desc, labelValues),
		createdTs:  timestamppb.New(ct),
	}, nil
}

// MustNewConstHistogramWithCreatedTimestamp is a version of NewConstHistogramWithCreatedTimestamp that panics where
// NewConstHistogramWithCreatedTimestamp would have returned an error.
func MustNewConstHistogramWithCreatedTimestamp(
	desc *Desc,
	count uint64,
	sum float64,
	buckets map[float64]uint64,
	ct time.Time,
	labelValues ...string,
) Metric {
	m, err := NewConstHistogramWithCreatedTimestamp(desc, count, sum, buckets, ct, labelValues...)
	if err != nil {
		panic(err)
	}
	return m
}

type buckSort []*dto.Bucket

func (s buckSort) Len() int {
	return len(s)
}

func (s buckSort) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s buckSort) Less(i, j int) bool {
	return s[i].GetUpperBound() < s[j].GetUpperBound()
}

// pickSchema returns the largest number n between -4 and 8 such that
// 2^(2^-n) is less or equal the provided bucketFactor.
//
// Special cases:
//   - bucketFactor <= 1: panics.
//   - bucketFactor < 2^(2^-8) (but > 1): still returns 8.
func pickSchema(bucketFactor float64) int32 {
	if bucketFactor <= 1 {
		panic(fmt.Errorf("bucketFactor %f is <=1", bucketFactor))
	}
	floor := math.Floor(math.Log2(math.Log2(bucketFactor)))
	switch {
	case floor <= -8:
		return 8
	case floor >= 4:
		return -4
	default:
		return -int32(floor)
	}
}

func makeBuckets(buckets *sync.Map) ([]*dto.BucketSpan, []int64) {
	var ii []int
	buckets.Range(func(k, v interface{}) bool {
		ii = append(ii, k.(int))
		return true
	})
	sort.Ints(ii)

	if len(ii) == 0 {
		return nil, nil
	}

	var (
		spans     []*dto.BucketSpan
		deltas    []int64
		prevCount int64
		nextI     int
	)

	appendDelta := func(count int64) {
		*spans[len(spans)-1].Length++
		deltas = append(deltas, count-prevCount)
		prevCount = count
	}

	for n, i := range ii {
		v, _ := buckets.Load(i)
		count := atomic.LoadInt64(v.(*int64))
		// Multiple spans with only small gaps in between are probably
		// encoded more efficiently as one larger span with a few empty
		// buckets. Needs some research to find the sweet spot. For now,
		// we assume that gaps of one or two buckets should not create
		// a new span.
		iDelta := int32(i - nextI)
		if n == 0 || iDelta > 2 {
			// We have to create a new span, either because we are
			// at the very beginning, or because we have found a gap
			// of more than two buckets.
			spans = append(spans, &dto.BucketSpan{
				Offset: proto.Int32(iDelta),
				Length: proto.Uint32(0),
			})
		} else {
			// We have found a small gap (or no gap at all).
			// Insert empty buckets as needed.
			for j := int32(0); j < iDelta; j++ {
				appendDelta(0)
			}
		}
		appendDelta(count)
		nextI = i + 1
	}
	return spans, deltas
}

// addToBucket increments the sparse bucket at key by the provided amount. It
// returns true if a new sparse bucket had to be created for that.
func addToBucket(buckets *sync.Map, key int, increment int64) bool {
	if existingBucket, ok := buckets.Load(key); ok {
		// Fast path without allocation.
		atomic.AddInt64(existingBucket.(*int64), increment)
		return false
	}
	// Bucket doesn't exist yet. Slow path allocating new counter.
	newBucket := increment // TODO(beorn7): Check if this is sufficient to not let increment escape.
	if actualBucket, loaded := buckets.LoadOrStore(key, &newBucket); loaded {
		// The bucket was created concurrently in another goroutine.
		// Have to increment after all.
		atomic.AddInt64(actualBucket.(*int64), increment)
		return false
	}
	return true
}

// addAndReset returns a function to be used with sync.Map.Range of spare
// buckets in coldCounts. It increments the buckets in the provided hotBuckets
// according to the buckets ranged through. It then resets all buckets ranged
// through to 0 (but leaves them in place so that they don't need to get
// recreated on the next scrape).
func addAndReset(hotBuckets *sync.Map, bucketNumber *uint32) func(k, v interface{}) bool {
	return func(k, v interface{}) bool {
		bucket := v.(*int64)
		if addToBucket(hotBuckets, k.(int), atomic.LoadInt64(bucket)) {
			atomic.AddUint32(bucketNumber, 1)
		}
		atomic.StoreInt64(bucket, 0)
		return true
	}
}

func deleteSyncMap(m *sync.Map) {
	m.Range(func(k, v interface{}) bool {
		m.Delete(k)
		return true
	})
}

func findSmallestKey(m *sync.Map) int {
	result := math.MaxInt32
	m.Range(func(k, v interface{}) bool {
		key := k.(int)
		if key < result {
			result = key
		}
		return true
	})
	return result
}

func getLe(key int, schema int32) float64 {
	// Here a bit of context about the behavior for the last bucket counting
	// regular numbers (called simply "last bucket" below) and the bucket
	// counting observations of ±Inf (called "inf bucket" below, with a key
	// one higher than that of the "last bucket"):
	//
	// If we apply the usual formula to the last bucket, its upper bound
	// would be calculated as +Inf. The reason is that the max possible
	// regular float64 number (math.MaxFloat64) doesn't coincide with one of
	// the calculated bucket boundaries. So the calculated boundary has to
	// be larger than math.MaxFloat64, and the only float64 larger than
	// math.MaxFloat64 is +Inf. However, we want to count actual
	// observations of ±Inf in the inf bucket. Therefore, we have to treat
	// the upper bound of the last bucket specially and set it to
	// math.MaxFloat64. (The upper bound of the inf bucket, with its key
	// being one higher than that of the last bucket, naturally comes out as
	// +Inf by the usual formula. So that's fine.)
	//
	// math.MaxFloat64 has a frac of 0.9999999999999999 and an exp of
	// 1024. If there were a float64 number following math.MaxFloat64, it
	// would have a frac of 1.0 and an exp of 1024, or equivalently a frac
	// of 0.5 and an exp of 1025. However, since frac must be smaller than
	// 1, and exp must be smaller than 1025, either representation overflows
	// a float64. (Which, in turn, is the reason that math.MaxFloat64 is the
	// largest possible float64. Q.E.D.) However, the formula for
	// calculating the upper bound from the idx and schema of the last
	// bucket results in precisely that. It is either frac=1.0 & exp=1024
	// (for schema < 0) or frac=0.5 & exp=1025 (for schema >=0). (This is,
	// by the way, a power of two where the exponent itself is a power of
	// two, 2¹⁰ in fact, which coinicides with a bucket boundary in all
	// schemas.) So these are the special cases we have to catch below.
	if schema < 0 {
		exp := key << -schema
		if exp == 1024 {
			// This is the last bucket before the overflow bucket
			// (for ±Inf observations). Return math.MaxFloat64 as
			// explained above.
			return math.MaxFloat64
		}
		return math.Ldexp(1, exp)
	}

	fracIdx := key & ((1 << schema) - 1)
	frac := nativeHistogramBounds[schema][fracIdx]
	exp := (key >> schema) + 1
	if frac == 0.5 && exp == 1025 {
		// This is the last bucket before the overflow bucket (for ±Inf
		// observations). Return math.MaxFloat64 as explained above.
		return math.MaxFloat64
	}
	return math.Ldexp(frac, exp)
}

// waitForCooldown returns after the count field in the provided histogramCounts
// has reached the provided count value.
func waitForCooldown(count uint64, counts *histogramCounts) {
	for count != atomic.LoadUint64(&counts.count) {
		runtime.Gosched() // Let observations get work done.
	}
}

// atomicAddFloat adds the provided float atomically to another float
// represented by the bit pattern the bits pointer is pointing to.
func atomicAddFloat(bits *uint64, v float64) {
	for {
		loadedBits := atomic.LoadUint64(bits)
		newBits := math.Float64bits(math.Float64frombits(loadedBits) + v)
		if atomic.CompareAndSwapUint64(bits, loadedBits, newBits) {
			break
		}
	}
}

// atomicDecUint32 atomically decrements the uint32 p points to.  See
// https://pkg.go.dev/sync/atomic#AddUint32 to understand how this is done.
func atomicDecUint32(p *uint32) {
	atomic.AddUint32(p, ^uint32(0))
}

// addAndResetCounts adds certain fields (count, sum, conventional buckets, zero
// bucket) from the cold counts to the corresponding fields in the hot
// counts. Those fields are then reset to 0 in the cold counts.
func addAndResetCounts(hot, cold *histogramCounts) {
	atomic.AddUint64(&hot.count, atomic.LoadUint64(&cold.count))
	atomic.StoreUint64(&cold.count, 0)
	coldSum := math.Float64frombits(atomic.LoadUint64(&cold.sumBits))
	atomicAddFloat(&hot.sumBits, coldSum)
	atomic.StoreUint64(&cold.sumBits, 0)
	for i := range hot.buckets {
		atomic.AddUint64(&hot.buckets[i], atomic.LoadUint64(&cold.buckets[i]))
		atomic.StoreUint64(&cold.buckets[i], 0)
	}
	atomic.AddUint64(&hot.nativeHistogramZeroBucket, atomic.LoadUint64(&cold.nativeHistogramZeroBucket))
	atomic.StoreUint64(&cold.nativeHistogramZeroBucket, 0)
}

type nativeExemplars struct {
	sync.Mutex

	// Time-to-live for exemplars, it is set to -1 if exemplars are disabled, that is NativeHistogramMaxExemplars is below 0.
	// The ttl is used on insertion to remove an exemplar that is older than ttl, if present.
	ttl time.Duration

	exemplars []*dto.Exemplar
}

func (n *nativeExemplars) isEnabled() bool {
	return n.ttl != -1
}

func makeNativeExemplars(ttl time.Duration, maxCount int) nativeExemplars {
	if ttl == 0 {
		ttl = 5 * time.Minute
	}

	if maxCount == 0 {
		maxCount = 10
	}

	if maxCount < 0 {
		maxCount = 0
		ttl = -1
	}

	return nativeExemplars{
		ttl:       ttl,
		exemplars: make([]*dto.Exemplar, 0, maxCount),
	}
}

func (n *nativeExemplars) addExemplar(e *dto.Exemplar) {
	if !n.isEnabled() {
		return
	}

	n.Lock()
	defer n.Unlock()

	// When the number of exemplars has not yet exceeded or
	// is equal to cap(n.exemplars), then
	// insert the new exemplar directly.
	if len(n.exemplars) < cap(n.exemplars) {
		var nIdx int
		for nIdx = 0; nIdx < len(n.exemplars); nIdx++ {
			if *e.Value < *n.exemplars[nIdx].Value {
				break
			}
		}
		n.exemplars = append(n.exemplars[:nIdx], append([]*dto.Exemplar{e}, n.exemplars[nIdx:]...)...)
		return
	}

	if len(n.exemplars) == 1 {
		// When the number of exemplars is 1, then
		// replace the existing exemplar with the new exemplar.
		n.exemplars[0] = e
		return
	}
	// From this point on, the number of exemplars is greater than 1.

	// When the number of exemplars exceeds the limit, remove one exemplar.
	var (
		ot    = time.Time{} // Oldest timestamp seen. Initial value doesn't matter as we replace it due to otIdx == -1 in the loop.
		otIdx = -1          // Index of the exemplar with the oldest timestamp.

		md = -1.0 // Logarithm of the delta of the closest pair of exemplars.

		// The insertion point of the new exemplar in the exemplars slice after insertion.
		// This is calculated purely based on the order of the exemplars by value.
		// nIdx == len(n.exemplars) means the new exemplar is to be inserted after the end.
		nIdx = -1

		// rIdx is ultimately the index for the exemplar that we are replacing with the new exemplar.
		// The aim is to keep a good spread of exemplars by value and not let them bunch up too much.
		// It is calculated in 3 steps:
		//   1. First we set rIdx to the index of the older exemplar within the closest pair by value.
		//      That is the following will be true (on log scale):
		//      either the exemplar pair on index (rIdx-1, rIdx) or (rIdx, rIdx+1) will have
		//      the closest values to each other from all pairs.
		//      For example, suppose the values are distributed like this:
		//        |-----------x-------------x----------------x----x-----|
		//                                                   ^--rIdx as this is older.
		//      Or like this:
		//        |-----------x-------------x----------------x----x-----|
		//                                                        ^--rIdx as this is older.
		//   2. If there is an exemplar that expired, then we simple reset rIdx to that index.
		//   3. We check if by inserting the new exemplar we would create a closer pair at
		//      (nIdx-1, nIdx) or (nIdx, nIdx+1) and set rIdx to nIdx-1 or nIdx accordingly to
		//      keep the spread of exemplars by value; otherwise we keep rIdx as it is.
		rIdx = -1
		cLog float64 // Logarithm of the current exemplar.
		pLog float64 // Logarithm of the previous exemplar.
	)

	for i, exemplar := range n.exemplars {
		// Find the exemplar with the oldest timestamp.
		if otIdx == -1 || exemplar.Timestamp.AsTime().Before(ot) {
			ot = exemplar.Timestamp.AsTime()
			otIdx = i
		}

		// Find the index at which to insert new the exemplar.
		if nIdx == -1 && *e.Value <= *exemplar.Value {
			nIdx = i
		}

		// Find the two closest exemplars and pick the one the with older timestamp.
		pLog = cLog
		cLog = math.Log(exemplar.GetValue())
		if i == 0 {
			continue
		}
		diff := math.Abs(cLog - pLog)
		if md == -1 || diff < md {
			// The closest exemplar pair is at index: i-1, i.
			// Choose the exemplar with the older timestamp for replacement.
			md = diff
			if n.exemplars[i].Timestamp.AsTime().Before(n.exemplars[i-1].Timestamp.AsTime()) {
				rIdx = i
			} else {
				rIdx = i - 1
			}
		}

	}

	// If all existing exemplar are smaller than new exemplar,
	// then the exemplar should be inserted at the end.
	if nIdx == -1 {
		nIdx = len(n.exemplars)
	}
	// Here, we have the following relationships:
	// n.exemplars[nIdx-1].Value < e.Value (if nIdx > 0)
	// e.Value <= n.exemplars[nIdx].Value (if nIdx < len(n.exemplars))

	if otIdx != -1 && e.Timestamp.AsTime().Sub(ot) > n.ttl {
		// If the oldest exemplar has expired, then replace it with the new exemplar.
		rIdx = otIdx
	} else {
		// In the previous for loop, when calculating the closest pair of exemplars,
		// we did not take into account the newly inserted exemplar.
		// So we need to calculate with the newly inserted exemplar again.
		elog := math.Log(e.GetValue())
		if nIdx > 0 {
			diff := math.Abs(elog - math.Log(n.exemplars[nIdx-1].GetValue()))
			if diff < md {
				// The value we are about to insert is closer to the previous exemplar at the insertion point than what we calculated before in rIdx.
				//                                            v--rIdx
				// |-----------x-n-----------x----------------x----x-----|
				//     nIdx-1--^ ^--new exemplar value
				// Do not make the spread worse, replace nIdx-1 and not rIdx.
				md = diff
				rIdx = nIdx - 1
			}
		}
		if nIdx < len(n.exemplars) {
			diff := math.Abs(math.Log(n.exemplars[nIdx].GetValue()) - elog)
			if diff < md {
				// The value we are about to insert is closer to the next exemplar at the insertion point than what we calculated before in rIdx.
				//                                            v--rIdx
				// |-----------x-----------n-x----------------x----x-----|
				//     new exemplar value--^ ^--nIdx
				// Do not make the spread worse, replace nIdx-1 and not rIdx.
				rIdx = nIdx
			}
		}
	}

	// Adjust the slice according to rIdx and nIdx.
	switch {
	case rIdx == nIdx:
		n.exemplars[nIdx] = e
	case rIdx < nIdx:
		n.exemplars = append(n.exemplars[:rIdx], append(n.exemplars[rIdx+1:nIdx], append([]*dto.Exemplar{e}, n.exemplars[nIdx:]...)...)...)
	case rIdx > nIdx:
		n.exemplars = append(n.exemplars[:nIdx], append([]*dto.Exemplar{e}, append(n.exemplars[nIdx:rIdx], n.exemplars[rIdx+1:]...)...)...)
	}
}
