package pathologicaleventlibrary

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	"github.com/openshift/origin/pkg/monitortestlibrary/platformidentification"

	v1 "github.com/openshift/api/config/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorv1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/openshift/origin/pkg/monitor/monitorapi"
	"github.com/openshift/origin/pkg/test/ginkgo/junitapi"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"
)

func TestDuplicatedEventForUpgrade(events monitorapi.Intervals, kubeClientConfig *rest.Config) []*junitapi.JUnitTestCase {
	// append upgrade allowances to the main slice
	allowedDupeEvents := []*AllowedDupeEvent{}
	allowedDupeEvents = append(allowedDupeEvents, AllowedRepeatedEvents...)
	allowedDupeEvents = append(allowedDupeEvents, AllowedRepeatedUpgradeEvents...)

	evaluator := duplicateEventsEvaluator{
		allowedDupeEvents: allowedDupeEvents,
	}

	if err := evaluator.getClusterInfo(kubeClientConfig); err != nil {
		e2e.Logf("could not fetch cluster info: %w", err)
	}

	tests := []*junitapi.JUnitTestCase{}
	tests = append(tests, evaluator.testDuplicatedCoreNamespaceEvents(events, kubeClientConfig)...)
	tests = append(tests, evaluator.testDuplicatedE2ENamespaceEvents(events, kubeClientConfig)...)
	return tests
}

func TestDuplicatedEventForStableSystem(events monitorapi.Intervals, clientConfig *rest.Config) []*junitapi.JUnitTestCase {

	evaluator := duplicateEventsEvaluator{
		allowedDupeEvents: AllowedRepeatedEvents,
	}

	/* TODO: restore, but it looks like this has been busted for ages, we append to a list of
	functions that are never used

	operatorClient, err := operatorv1client.NewForConfig(clientConfig)
	if err != nil {
		panic(err)
	}
	etcdAllowance, err := newDuplicatedEventsAllowedWhenEtcdRevisionChange(context.TODO(), operatorClient)
	if err != nil {
		panic(fmt.Errorf("unable to construct duplicated events allowance for etcd, err = %v", err))
	}
	evaluator.allowedRepeatedEventFns = append(evaluator.allowedRepeatedEventFns, etcdAllowance.allowEtcdGuardReadinessProbeFailure)
	*/

	if err := evaluator.getClusterInfo(clientConfig); err != nil {
		e2e.Logf("could not fetch cluster info: %w", err)
	}

	tests := []*junitapi.JUnitTestCase{}
	tests = append(tests, evaluator.testDuplicatedCoreNamespaceEvents(events, clientConfig)...)
	tests = append(tests, evaluator.testDuplicatedE2ENamespaceEvents(events, clientConfig)...)
	return tests
}

func combinedRegexp(arr ...*regexp.Regexp) *regexp.Regexp {
	s := ""
	for _, r := range arr {
		if s != "" {
			s += "|"
		}
		s += r.String()
	}
	return regexp.MustCompile(s)
}

type duplicateEventsEvaluator struct {
	// allowedDupeEvents is the list of matchers we use to see if a repeat kube event is allowed or not.
	allowedDupeEvents []*AllowedDupeEvent

	// platform contains the current platform of the cluster under Test.
	platform v1.PlatformType

	// topology contains the topology of the cluster under Test.
	topology v1.TopologyMode
}

// we want to identify events based on the monitor because it is (currently) our only spot that tracks events over time
// for every run. this means we see events that are created during updates and in e2e tests themselves.  A [late] Test
// is easier to author, but less complete in its view.
// I hate regexes, so I only do this because I really have to.
func (d *duplicateEventsEvaluator) testDuplicatedCoreNamespaceEvents(events monitorapi.Intervals, kubeClientConfig *rest.Config) []*junitapi.JUnitTestCase {
	const testName = "[sig-arch] events should not repeat pathologically"

	return d.testDuplicatedEvents(testName, false, events.Filter(monitorapi.Not(monitorapi.IsInE2ENamespace)), kubeClientConfig, false)
}

// we want to identify events based on the monitor because it is (currently) our only spot that tracks events over time
// for every run. this means we see events that are created during updates and in e2e tests themselves.  A [late] Test
// is easier to author, but less complete in its view.
// I hate regexes, so I only do this because I really have to.
func (d *duplicateEventsEvaluator) testDuplicatedE2ENamespaceEvents(events monitorapi.Intervals, kubeClientConfig *rest.Config) []*junitapi.JUnitTestCase {
	const testName = "[sig-arch] events should not repeat pathologically in e2e namespaces"

	return d.testDuplicatedEvents(testName, true, events.Filter(monitorapi.IsInE2ENamespace), kubeClientConfig, true)
}

// appendToFirstLine appends add to the end of the first line of s
func appendToFirstLine(s string, add string) string {
	splits := strings.Split(s, "\n")
	splits[0] += add
	return strings.Join(splits, "\n")
}

func getJUnitName(testName string, namespace string) string {
	jUnitName := testName
	if namespace != "" {
		jUnitName = jUnitName + " for ns/" + namespace
	}
	return jUnitName
}

func getNamespacesForJUnits() sets.String {
	namespaces := platformidentification.KnownNamespaces.Clone()
	namespaces.Insert("")
	return namespaces
}

type eventResult struct {
	failures []string
	flakes   []string
}

func generateFailureOutput(failures []string, flakes []string) string {
	var output string
	if len(failures) > 0 {
		output = fmt.Sprintf("%d events happened too frequently\n\n%v", len(failures), strings.Join(failures, "\n"))
	}
	if len(flakes) > 0 {
		if output != "" {
			output += "\n\n"
		}
		output += fmt.Sprintf("%d events with known BZs\n\n%v", len(flakes), strings.Join(flakes, "\n"))
	}
	return output
}

func generateJUnitTestCasesCoreNamespaces(testName string, nsResults map[string]*eventResult) []*junitapi.JUnitTestCase {
	var tests []*junitapi.JUnitTestCase
	namespaces := getNamespacesForJUnits()
	for namespace := range namespaces {
		jUnitName := getJUnitName(testName, namespace)
		if result, ok := nsResults[namespace]; ok {
			output := generateFailureOutput(result.failures, result.flakes)
			tests = append(tests, &junitapi.JUnitTestCase{
				Name: jUnitName,
				FailureOutput: &junitapi.FailureOutput{
					Output: output,
				},
			})
			// Add a success for flakes
			if len(result.failures) == 0 && len(result.flakes) > 0 {
				tests = append(tests, &junitapi.JUnitTestCase{Name: jUnitName})
			}
		} else {
			tests = append(tests, &junitapi.JUnitTestCase{Name: jUnitName})
		}
	}
	return tests
}

func generateJUnitTestCasesE2ENamespaces(testName string, nsResults map[string]*eventResult) []*junitapi.JUnitTestCase {
	var tests []*junitapi.JUnitTestCase
	if result, ok := nsResults[""]; ok {
		if len(result.failures) > 0 || len(result.flakes) > 0 {
			output := generateFailureOutput(result.failures, result.flakes)
			tests = append(tests, &junitapi.JUnitTestCase{
				Name: testName,
				FailureOutput: &junitapi.FailureOutput{
					Output: output,
				},
			})
		}
		if len(result.failures) == 0 {
			// Add success for flake
			tests = append(tests, &junitapi.JUnitTestCase{Name: testName})
		}
	}
	if len(tests) == 0 {
		tests = append(tests, &junitapi.JUnitTestCase{Name: testName})
	}
	return tests
}

// we want to identify events based on the monitor because it is (currently) our only spot that tracks events over time
// for every run. this means we see events that are created during updates and in e2e tests themselves.  A [late] Test
// is easier to author, but less complete in its view.
// I hate regexes, so I only do this because I really have to.
func (d *duplicateEventsEvaluator) testDuplicatedEvents(testName string, flakeOnly bool, events monitorapi.Intervals, kubeClientConfig *rest.Config, isE2E bool) []*junitapi.JUnitTestCase {

	// Filter out a list of NodeUpdate events, we use these to ignore some other potential pathological events that are
	// expected during NodeUpdate.
	nodeUpdateIntervals := events.Filter(func(eventInterval monitorapi.Interval) bool {
		return eventInterval.Source == monitorapi.SourceNodeState &&
			eventInterval.StructuredLocator.Type == monitorapi.LocatorTypeNode &&
			eventInterval.StructuredMessage.Annotations[monitorapi.AnnotationConstructed] == monitorapi.ConstructionOwnerNodeLifecycle &&
			eventInterval.StructuredMessage.Annotations[monitorapi.AnnotationPhase] == "Update" &&
			strings.Contains(eventInterval.StructuredMessage.Annotations[monitorapi.AnnotationRoles], "master")
	})
	logrus.Infof("found %d NodeUpdate intervals", len(nodeUpdateIntervals))

	type timeRange struct {
		from time.Time
		to   time.Time
	}
	buildTopologyHintAllowedTimeRanges := true
	topologyHintAllowedTimeRanges := []*timeRange{}

	// displayToCount maps a static display message to the matching repeating interval we saw with the highest count
	displayToCount := map[string]monitorapi.Interval{}

	for _, event := range events {
		// TODO: would be nice to move these two exceptions to duplicated_event_patterns, but it's tricky for them
		// to only calculate the data they need one time...
		if event.StructuredMessage.Reason == "FailedScheduling" {
			// Filter out FailedScheduling events while masters are updating
			var foundOverlap bool
			for _, nui := range nodeUpdateIntervals {
				if nui.From.Before(event.From) && nui.To.After(event.To) {
					logrus.Infof("%s was found to overlap with %s, ignoring pathological event as we expect these during master updates", event, nui)
					foundOverlap = true
					break
				}
			}
			if foundOverlap {
				continue
			}
		}

		if event.StructuredMessage.Reason == "TopologyAwareHintsDisabled" {
			// Build the allowed time range only once
			if buildTopologyHintAllowedTimeRanges {
				taintManagerTestIntervals := events.Filter(func(eventInterval monitorapi.Interval) bool {
					return eventInterval.Source == monitorapi.SourceE2ETest &&
						strings.Contains(eventInterval.StructuredLocator.Keys[monitorapi.LocatorE2ETestKey], "NoExecuteTaintManager")
				})
				// Start the allowed time range from time range of the tests. But events lag behind the tests since the tests do not wait
				// until all dns pods are properly scheduled and reach ready state. So we will need to expand the allowed time range after.
				for _, test := range taintManagerTestIntervals {
					topologyHintAllowedTimeRanges = append(topologyHintAllowedTimeRanges, &timeRange{from: test.From, to: test.To})
					logrus.WithField("from", test.From).WithField("to", test.To).Infof("found time range for test: %s", testName)
				}
				dnsUpdateIntervals := events.Filter(func(eventInterval monitorapi.Interval) bool {
					return eventInterval.Source == monitorapi.SourcePodState &&
						(eventInterval.StructuredLocator.Type == monitorapi.LocatorTypePod || eventInterval.StructuredLocator.Type == monitorapi.LocatorTypeContainer) &&
						eventInterval.StructuredLocator.Keys[monitorapi.LocatorNamespaceKey] == "openshift-dns" &&
						eventInterval.StructuredMessage.Annotations[monitorapi.AnnotationConstructed] == monitorapi.ConstructionOwnerPodLifecycle
				})

				// Now expand the allowed time range until the replacement dns pod gets ready
				for _, r := range topologyHintAllowedTimeRanges {
					var lastReadyTime time.Time
					count := 0
					for _, interval := range dnsUpdateIntervals {
						if interval.From.Before(r.from) {
							continue
						}
						// If there is a GracefulDelete of dns-default pod, we will have to wait until the replacement dns container becomes ready
						if interval.StructuredMessage.Reason == monitorapi.PodReasonGracefulDeleteStarted &&
							strings.Contains(interval.StructuredLocator.Keys[monitorapi.LocatorPodKey], "dns-default") {
							count++
						}
						if interval.StructuredMessage.Reason == monitorapi.ContainerReasonReady &&
							interval.StructuredLocator.Keys[monitorapi.LocatorContainerKey] == "dns" && count > 0 {
							lastReadyTime = interval.From
							count--
						}
						if interval.From.After(r.to) && count == 0 {
							if lastReadyTime.After(r.to) {
								r.to = lastReadyTime
							}
							break
						}
					}
				}
				// Log final adjusted time ranges
				for _, test := range taintManagerTestIntervals {
					logrus.WithField("from", test.From).WithField("to", test.To).Infof("adjusted time range for test: %s", testName)
				}
				buildTopologyHintAllowedTimeRanges = false
			}
			// Filter out TopologyAwareHintsDisabled events within allowed time range
			var allowed bool
			for _, r := range topologyHintAllowedTimeRanges {
				if r.from.Before(event.From) && r.to.After(event.To) {
					logrus.Infof("%s was found to fall into the allowed time range %+v, ignoring pathological event as we expect these during NoExecuteTaintManager test", event, r)
					allowed = true
					break
				}
			}
			if allowed {
				continue
			}
		}

		times := GetTimesAnEventHappened(event.StructuredMessage)
		if times > DuplicateEventThreshold {

			// Check if we have an allowance for this event. This code used to just check if it had an interesting flag,
			// implying it matches some pattern, but that happens even for upgrade patterns occurring in non-upgrade jobs,
			// so we were ignoring patterns that were meant to be allowed only in upgrade jobs in all jobs. The list of
			// allowed patterns passed to this object wasn't even used.
			if allowed, _ := MatchesAny(d.allowedDupeEvents, event.StructuredLocator, event.StructuredMessage, kubeClientConfig, &d.topology); allowed {
				continue
			}

			// key used in a map to identify the common interval that is repeating and we may
			// encounter multiple times.
			eventDisplayMessage := fmt.Sprintf("%s - reason/%s %s", event.Locator,
				event.StructuredMessage.Reason, event.StructuredMessage.HumanMessage)

			if _, ok := displayToCount[eventDisplayMessage]; !ok {
				displayToCount[eventDisplayMessage] = event
			}
			if times > GetTimesAnEventHappened(displayToCount[eventDisplayMessage].StructuredMessage) {
				// Update to the latest interval we saw with the higher count, so from/to are more accurate
				displayToCount[eventDisplayMessage] = event
			}
		}
	}

	nsResults := map[string]*eventResult{}
	for intervalDisplayMsg, interval := range displayToCount {
		namespace := interval.StructuredLocator.Keys[monitorapi.LocatorNamespaceKey]
		intervalMsgWithTime := intervalDisplayMsg + " From: " + interval.From.Format("15:04:05Z") + " To: " + interval.To.Format("15:04:05Z")
		msg := fmt.Sprintf("event happened %d times, something is wrong: %v",
			GetTimesAnEventHappened(interval.StructuredMessage), intervalMsgWithTime)

		// We only create junit for known namespaces
		if !platformidentification.KnownNamespaces.Has(namespace) {
			namespace = ""
		}

		if _, ok := nsResults[namespace]; !ok {
			tmp := &eventResult{}
			nsResults[namespace] = tmp
		}
		if flakeOnly {
			nsResults[namespace].flakes = append(nsResults[namespace].flakes, appendToFirstLine(msg, " result=allow "))
		} else {
			nsResults[namespace].failures = append(nsResults[namespace].failures, appendToFirstLine(msg, " result=reject "))
		}
	}

	var tests []*junitapi.JUnitTestCase
	if isE2E {
		tests = generateJUnitTestCasesE2ENamespaces(testName, nsResults)
	} else {
		tests = generateJUnitTestCasesCoreNamespaces(testName, nsResults)
	}
	return tests
}

func GetTimesAnEventHappened(msg monitorapi.Message) int {
	countStr, ok := msg.Annotations[monitorapi.AnnotationCount]
	if !ok {
		return 1
	}
	times, err := strconv.ParseInt(countStr, 10, 0)
	if err != nil { // not an int somehow
		logrus.Warnf("interval had a non-integer count? %+v", msg)
		return 0
	}
	return int(times)
}

func (d *duplicateEventsEvaluator) getClusterInfo(c *rest.Config) (err error) {
	if c == nil {
		return
	}

	oc, err := configclient.NewForConfig(c)
	if err != nil {
		return err
	}
	infra, err := oc.ConfigV1().Infrastructures().Get(context.Background(), "cluster", metav1.GetOptions{})
	if err != nil {
		return err
	}

	if infra.Status.PlatformStatus != nil && infra.Status.PlatformStatus.Type != "" {
		d.platform = infra.Status.PlatformStatus.Type
	}

	if infra.Status.ControlPlaneTopology != "" {
		d.topology = infra.Status.ControlPlaneTopology
	}

	return nil
}

type etcdRevisionChangeAllowance struct {
	allowedGuardProbeFailurePattern        *regexp.Regexp
	maxAllowedGuardProbeFailurePerRevision int

	currentRevision int
}

func newDuplicatedEventsAllowedWhenEtcdRevisionChange(ctx context.Context, operatorClient operatorv1client.OperatorV1Interface) (*etcdRevisionChangeAllowance, error) {
	currentRevision, err := getBiggestRevisionForEtcdOperator(ctx, operatorClient)
	if err != nil {
		return nil, err
	}
	return &etcdRevisionChangeAllowance{
		allowedGuardProbeFailurePattern:        regexp.MustCompile(`ns/openshift-etcd pod/etcd-guard-.* node/[a-z0-9.-]+ - reason/(Unhealthy|ProbeError) Readiness probe.*`),
		maxAllowedGuardProbeFailurePerRevision: 60 / 5, // 60s for starting a new pod, divided by the probe interval
		currentRevision:                        currentRevision,
	}, nil
}

// allowEtcdGuardReadinessProbeFailure tolerates events that match allowedGuardProbeFailurePattern unless we receive more than a.maxAllowedGuardProbeFailurePerRevision*a.currentRevision
func (a *etcdRevisionChangeAllowance) allowEtcdGuardReadinessProbeFailure(monitorEvent monitorapi.Interval, _ *rest.Config, times int) (bool, error) {
	eventMessage := fmt.Sprintf("%s - %s", monitorEvent.Locator, monitorEvent.Message)

	// allow for a.maxAllowedGuardProbeFailurePerRevision * a.currentRevision failed readiness probe from the etcd-guard pods
	// since the guards are static and the etcd pods come and go during a rollout
	// which causes allowedGuardProbeFailurePattern to fire
	if a.allowedGuardProbeFailurePattern.MatchString(eventMessage) && a.maxAllowedGuardProbeFailurePerRevision*a.currentRevision > times {
		return true, nil
	}
	return false, nil
}

// getBiggestRevisionForEtcdOperator calculates the biggest revision among replicas of the most recently successful deployment
func getBiggestRevisionForEtcdOperator(ctx context.Context, operatorClient operatorv1client.OperatorV1Interface) (int, error) {
	etcd, err := operatorClient.Etcds().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		// instead of panicking when there no etcd operator (e.g. microshift), just estimate the biggest revision to be 0
		if apierrors.IsNotFound(err) {
			return 0, nil
		} else {
			return 0, err
		}

	}
	biggestRevision := 0
	for _, nodeStatus := range etcd.Status.NodeStatuses {
		if int(nodeStatus.CurrentRevision) > biggestRevision {
			biggestRevision = int(nodeStatus.CurrentRevision)
		}
	}
	return biggestRevision, nil
}

// BuildTestDupeKubeEvent is a test utility to make the process of creating these specific intervals a little
// more brief.
func BuildTestDupeKubeEvent(namespace, pod, reason, msg string, count int) monitorapi.Interval {

	l := monitorapi.Locator{
		Type: monitorapi.LocatorTypePod,
		Keys: map[monitorapi.LocatorKey]string{},
	}
	if namespace != "" {
		l.Keys[monitorapi.LocatorNamespaceKey] = namespace
	}
	if pod != "" {
		l.Keys[monitorapi.LocatorPodKey] = namespace
	}

	i := monitorapi.NewInterval(monitorapi.SourceKubeEvent, monitorapi.Info).
		Locator(l).
		Message(
			monitorapi.NewMessage().
				Reason(monitorapi.IntervalReason(reason)).
				HumanMessage(msg).
				WithAnnotation(monitorapi.AnnotationCount, fmt.Sprintf("%d", count))).
		BuildNow()

	return i
}
