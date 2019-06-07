package influxdb

import (
	"fmt"
	"strings"

	"github.com/influxdata/flux"
	"github.com/influxdata/flux/ast"
	"github.com/influxdata/flux/execute"
	"github.com/influxdata/flux/plan"
	"github.com/influxdata/flux/semantic"
	"github.com/influxdata/flux/stdlib/influxdata/influxdb"
	"github.com/influxdata/flux/stdlib/universe"
	"github.com/influxdata/flux/values"
	"github.com/influxdata/influxdb/services/meta"
	"github.com/influxdata/influxql"
	"github.com/pkg/errors"
)

const FromKind = "influxDBFrom"

type FromOpSpec struct {
	Bucket   string `json:"bucket,omitempty"`
	BucketID string `json:"bucketID,omitempty"`
}

func init() {
	fromSignature := semantic.FunctionPolySignature{
		Parameters: map[string]semantic.PolyType{
			"bucket":   semantic.String,
			"bucketID": semantic.String,
		},
		Required: nil,
		Return:   flux.TableObjectType,
	}

	flux.ReplacePackageValue("influxdata/influxdb", influxdb.FromKind, flux.FunctionValue(FromKind, createFromOpSpec, fromSignature))
	flux.RegisterOpSpec(FromKind, newFromOp)
	plan.RegisterProcedureSpec(FromKind, newFromProcedure, FromKind)
	plan.RegisterPhysicalRules(
		FromConversionRule{},
		MergeFromRangeRule{},
		MergeFromFilterRule{},
		FromDistinctRule{},
		MergeFromGroupRule{},
		FromKeysRule{},
	)
	execute.RegisterSource(PhysicalFromKind, createFromSource)
}

func createFromOpSpec(args flux.Arguments, a *flux.Administration) (flux.OperationSpec, error) {
	spec := new(FromOpSpec)

	if bucket, ok, err := args.GetString("bucket"); err != nil {
		return nil, err
	} else if ok {
		spec.Bucket = bucket
	}

	if bucketID, ok, err := args.GetString("bucketID"); err != nil {
		return nil, err
	} else if ok {
		spec.BucketID = bucketID
	}

	if spec.Bucket == "" && spec.BucketID == "" {
		return nil, errors.New("must specify one of bucket or bucketID")
	}
	if spec.Bucket != "" && spec.BucketID != "" {
		return nil, errors.New("must specify only one of bucket or bucketID")
	}
	return spec, nil
}

func newFromOp() flux.OperationSpec {
	return new(FromOpSpec)
}

func (s *FromOpSpec) Kind() flux.OperationKind {
	return FromKind
}

type FromProcedureSpec struct {
	Bucket   string
	BucketID string
}

func newFromProcedure(qs flux.OperationSpec, pa plan.Administration) (plan.ProcedureSpec, error) {
	spec, ok := qs.(*FromOpSpec)
	if !ok {
		return nil, fmt.Errorf("invalid spec type %T", qs)
	}

	return &FromProcedureSpec{
		Bucket:   spec.Bucket,
		BucketID: spec.BucketID,
	}, nil
}

func (s *FromProcedureSpec) Kind() plan.ProcedureKind {
	return FromKind
}

func (s *FromProcedureSpec) Copy() plan.ProcedureSpec {
	ns := new(FromProcedureSpec)

	ns.Bucket = s.Bucket
	ns.BucketID = s.BucketID

	return ns
}

func (s FromProcedureSpec) PostPhysicalValidate(id plan.NodeID) error {
	// FromProcedureSpec has no bounds, so must be invalid.
	var bucket string
	if len(s.Bucket) > 0 {
		bucket = s.Bucket
	} else {
		bucket = s.BucketID
	}
	return fmt.Errorf(`%s: results from "%s" must be bounded`, id, bucket)
}

const PhysicalFromKind = "physFrom"

type PhysicalFromProcedureSpec struct {
	FromProcedureSpec

	plan.DefaultCost
	BoundsSet bool
	Bounds    flux.Bounds

	FilterSet bool
	Filter    *semantic.FunctionExpression

	DescendingSet bool
	Descending    bool

	LimitSet     bool
	PointsLimit  int64
	SeriesLimit  int64
	SeriesOffset int64

	WindowSet bool
	Window    plan.WindowSpec

	GroupingSet bool
	OrderByTime bool
	GroupMode   flux.GroupMode
	GroupKeys   []string

	AggregateSet    bool
	AggregateMethod string
}

func (PhysicalFromProcedureSpec) Kind() plan.ProcedureKind {
	return PhysicalFromKind
}

func (s *PhysicalFromProcedureSpec) Copy() plan.ProcedureSpec {
	ns := new(PhysicalFromProcedureSpec)

	ns.Bucket = s.Bucket
	ns.BucketID = s.BucketID

	ns.BoundsSet = s.BoundsSet
	ns.Bounds = s.Bounds

	ns.FilterSet = s.FilterSet
	ns.Filter = s.Filter.Copy().(*semantic.FunctionExpression)

	ns.DescendingSet = s.DescendingSet
	ns.Descending = s.Descending

	ns.LimitSet = s.LimitSet
	ns.PointsLimit = s.PointsLimit
	ns.SeriesLimit = s.SeriesLimit
	ns.SeriesOffset = s.SeriesOffset

	ns.WindowSet = s.WindowSet
	ns.Window = s.Window

	ns.GroupingSet = s.GroupingSet
	ns.OrderByTime = s.OrderByTime
	ns.GroupMode = s.GroupMode
	ns.GroupKeys = s.GroupKeys

	ns.AggregateSet = s.AggregateSet
	ns.AggregateMethod = s.AggregateMethod

	return ns
}

// TimeBounds implements plan.BoundsAwareProcedureSpec.
func (s *PhysicalFromProcedureSpec) TimeBounds(predecessorBounds *plan.Bounds) *plan.Bounds {
	if s.BoundsSet {
		bounds := &plan.Bounds{
			Start: values.ConvertTime(s.Bounds.Start.Time(s.Bounds.Now)),
			Stop:  values.ConvertTime(s.Bounds.Stop.Time(s.Bounds.Now)),
		}
		return bounds
	}
	return nil
}

func (s PhysicalFromProcedureSpec) PostPhysicalValidate(id plan.NodeID) error {
	if !s.BoundsSet || (s.Bounds.Start.IsZero() && s.Bounds.Stop.IsZero()) {
		var bucket string
		if len(s.Bucket) > 0 {
			bucket = s.Bucket
		} else {
			bucket = s.BucketID
		}
		return fmt.Errorf(`%s: results from "%s" must be bounded`, id, bucket)
	}

	return nil
}

// FromConversionRule converts a logical `from` node into a physical `from` node.
// TODO(cwolff): this rule can go away when we require a `range`
//  to be pushed into a logical `from` to create a physical `from.`
type FromConversionRule struct {
}

func (FromConversionRule) Name() string {
	return "FromConversionRule"
}

func (FromConversionRule) Pattern() plan.Pattern {
	return plan.Pat(FromKind)
}

func (FromConversionRule) Rewrite(pn plan.Node) (plan.Node, bool, error) {
	logicalFromSpec := pn.ProcedureSpec().(*FromProcedureSpec)
	newNode := plan.CreatePhysicalNode(pn.ID(), &PhysicalFromProcedureSpec{
		FromProcedureSpec: *logicalFromSpec,
	})

	plan.ReplaceNode(pn, newNode)
	return newNode, true, nil
}

// MergeFromRangeRule pushes a `range` into a `from`.
type MergeFromRangeRule struct{}

// Name returns the name of the rule.
func (rule MergeFromRangeRule) Name() string {
	return "MergeFromRangeRule"
}

// Pattern returns the pattern that matches `from -> range`.
func (rule MergeFromRangeRule) Pattern() plan.Pattern {
	return plan.Pat(universe.RangeKind, plan.Pat(PhysicalFromKind))
}

// Rewrite attempts to rewrite a `from -> range` into a `FromRange`.
func (rule MergeFromRangeRule) Rewrite(node plan.Node) (plan.Node, bool, error) {
	from := node.Predecessors()[0]
	fromSpec := from.ProcedureSpec().(*PhysicalFromProcedureSpec)
	rangeSpec := node.ProcedureSpec().(*universe.RangeProcedureSpec)
	fromRange := fromSpec.Copy().(*PhysicalFromProcedureSpec)

	// Set new bounds to `range` bounds initially
	fromRange.Bounds = rangeSpec.Bounds

	var (
		now   = rangeSpec.Bounds.Now
		start = rangeSpec.Bounds.Start
		stop  = rangeSpec.Bounds.Stop
	)

	bounds := &plan.Bounds{
		Start: values.ConvertTime(start.Time(now)),
		Stop:  values.ConvertTime(stop.Time(now)),
	}

	// Intersect bounds if `from` already bounded
	if fromSpec.BoundsSet {
		now = fromSpec.Bounds.Now
		start = fromSpec.Bounds.Start
		stop = fromSpec.Bounds.Stop

		fromBounds := &plan.Bounds{
			Start: values.ConvertTime(start.Time(now)),
			Stop:  values.ConvertTime(stop.Time(now)),
		}

		bounds = bounds.Intersect(fromBounds)
		fromRange.Bounds = flux.Bounds{
			Start: flux.Time{Absolute: bounds.Start.Time()},
			Stop:  flux.Time{Absolute: bounds.Stop.Time()},
		}
	}

	fromRange.BoundsSet = true

	// Finally merge nodes into single operation
	merged, err := plan.MergeToPhysicalNode(node, from, fromRange)
	if err != nil {
		return nil, false, err
	}

	return merged, true, nil
}

// MergeFromFilterRule is a rule that pushes filters into from procedures to be evaluated in the storage layer.
// This rule is likely to be replaced by a more generic rule when we have a better
// framework for pushing filters, etc into sources.
type MergeFromFilterRule struct{}

func (MergeFromFilterRule) Name() string {
	return "MergeFromFilterRule"
}

func (MergeFromFilterRule) Pattern() plan.Pattern {
	return plan.Pat(universe.FilterKind, plan.Pat(PhysicalFromKind))
}

func (MergeFromFilterRule) Rewrite(filterNode plan.Node) (plan.Node, bool, error) {
	filterSpec := filterNode.ProcedureSpec().(*universe.FilterProcedureSpec)
	fromNode := filterNode.Predecessors()[0]
	fromSpec := fromNode.ProcedureSpec().(*PhysicalFromProcedureSpec)

	if fromSpec.AggregateSet || fromSpec.GroupingSet {
		return filterNode, false, nil
	}

	bodyExpr, ok := filterSpec.Fn.Block.Body.(semantic.Expression)
	if !ok {
		return filterNode, false, nil
	}

	if len(filterSpec.Fn.Block.Parameters.List) != 1 {
		// I would expect that type checking would catch this, but just to be safe...
		return filterNode, false, nil
	}

	paramName := filterSpec.Fn.Block.Parameters.List[0].Key.Name

	pushable, notPushable, err := semantic.PartitionPredicates(bodyExpr, func(e semantic.Expression) (bool, error) {
		return isPushableExpr(paramName, e)
	})
	if err != nil {
		return nil, false, err
	}

	if pushable == nil {
		// Nothing could be pushed down, no rewrite can happen
		return filterNode, false, nil
	}

	newFromSpec := fromSpec.Copy().(*PhysicalFromProcedureSpec)
	if newFromSpec.FilterSet {
		newBody := semantic.ExprsToConjunction(newFromSpec.Filter.Block.Body.(semantic.Expression), pushable)
		newFromSpec.Filter.Block.Body = newBody
	} else {
		newFromSpec.FilterSet = true
		newFromSpec.Filter = filterSpec.Fn.Copy().(*semantic.FunctionExpression)
		newFromSpec.Filter.Block.Body = pushable
	}

	if notPushable == nil {
		// All predicates could be pushed down, so eliminate the filter
		mergedNode, err := plan.MergeToPhysicalNode(filterNode, fromNode, newFromSpec)
		if err != nil {
			return nil, false, err
		}
		return mergedNode, true, nil
	}

	err = fromNode.ReplaceSpec(newFromSpec)
	if err != nil {
		return nil, false, err
	}

	newFilterSpec := filterSpec.Copy().(*universe.FilterProcedureSpec)
	newFilterSpec.Fn.Block.Body = notPushable
	err = filterNode.ReplaceSpec(newFilterSpec)
	if err != nil {
		return nil, false, err
	}

	return filterNode, true, nil
}

// isPushableExpr determines if a predicate expression can be pushed down into the storage layer.
func isPushableExpr(paramName string, expr semantic.Expression) (bool, error) {
	switch e := expr.(type) {
	case *semantic.LogicalExpression:
		b, err := isPushableExpr(paramName, e.Left)
		if err != nil {
			return false, err
		}

		if !b {
			return false, nil
		}

		return isPushableExpr(paramName, e.Right)

	case *semantic.BinaryExpression:
		if isPushablePredicate(paramName, e) {
			return true, nil
		}
	}

	return false, nil
}

func isPushablePredicate(paramName string, be *semantic.BinaryExpression) bool {
	// Manual testing seems to indicate that (at least right now) we can
	// only handle predicates of the form <fn param>.<property> <op> <literal>
	// and the literal must be on the RHS.

	if !isLiteral(be.Right) {
		return false
	}

	if isField(paramName, be.Left) && isPushableFieldOperator(be.Operator) {
		return true
	}

	if isTag(paramName, be.Left) && isPushableTagOperator(be.Operator) {
		return true
	}

	return false
}

func isLiteral(e semantic.Expression) bool {
	switch e.(type) {
	case *semantic.StringLiteral:
		return true
	case *semantic.IntegerLiteral:
		return true
	case *semantic.BooleanLiteral:
		return true
	case *semantic.FloatLiteral:
		return true
	case *semantic.RegexpLiteral:
		return true
	}

	return false
}

const fieldValueProperty = "_value"

func isTag(paramName string, e semantic.Expression) bool {
	memberExpr := validateMemberExpr(paramName, e)
	return memberExpr != nil && memberExpr.Property != fieldValueProperty
}

func isField(paramName string, e semantic.Expression) bool {
	memberExpr := validateMemberExpr(paramName, e)
	return memberExpr != nil && memberExpr.Property == fieldValueProperty
}

func validateMemberExpr(paramName string, e semantic.Expression) *semantic.MemberExpression {
	memberExpr, ok := e.(*semantic.MemberExpression)
	if !ok {
		return nil
	}

	idExpr, ok := memberExpr.Object.(*semantic.IdentifierExpression)
	if !ok {
		return nil
	}

	if idExpr.Name != paramName {
		return nil
	}

	return memberExpr
}

func isPushableTagOperator(kind ast.OperatorKind) bool {
	pushableOperators := []ast.OperatorKind{
		ast.EqualOperator,
		ast.NotEqualOperator,
		ast.RegexpMatchOperator,
		ast.NotRegexpMatchOperator,
	}

	for _, op := range pushableOperators {
		if op == kind {
			return true
		}
	}

	return false
}

func isPushableFieldOperator(kind ast.OperatorKind) bool {
	if isPushableTagOperator(kind) {
		return true
	}

	// Fields can be filtered by anything that tags can be filtered by,
	// plus range operators.

	moreOperators := []ast.OperatorKind{
		ast.LessThanEqualOperator,
		ast.LessThanOperator,
		ast.GreaterThanEqualOperator,
		ast.GreaterThanOperator,
	}

	for _, op := range moreOperators {
		if op == kind {
			return true
		}
	}

	return false
}

type FromDistinctRule struct {
}

func (FromDistinctRule) Name() string {
	return "FromDistinctRule"
}

func (FromDistinctRule) Pattern() plan.Pattern {
	return plan.Pat(universe.DistinctKind, plan.Pat(PhysicalFromKind))
}

func (FromDistinctRule) Rewrite(distinctNode plan.Node) (plan.Node, bool, error) {
	fromNode := distinctNode.Predecessors()[0]
	distinctSpec := distinctNode.ProcedureSpec().(*universe.DistinctProcedureSpec)
	fromSpec := fromNode.ProcedureSpec().(*PhysicalFromProcedureSpec)

	if fromSpec.LimitSet && fromSpec.PointsLimit == -1 {
		return distinctNode, false, nil
	}

	groupStar := !fromSpec.GroupingSet && distinctSpec.Column != execute.DefaultValueColLabel && distinctSpec.Column != execute.DefaultTimeColLabel
	groupByColumn := fromSpec.GroupingSet && len(fromSpec.GroupKeys) > 0 &&
		((fromSpec.GroupMode == flux.GroupModeBy && execute.ContainsStr(fromSpec.GroupKeys, distinctSpec.Column)) ||
			(fromSpec.GroupMode == flux.GroupModeExcept && !execute.ContainsStr(fromSpec.GroupKeys, distinctSpec.Column)))
	if groupStar || groupByColumn {
		newFromSpec := fromSpec.Copy().(*PhysicalFromProcedureSpec)
		newFromSpec.LimitSet = true
		newFromSpec.PointsLimit = -1
		if err := fromNode.ReplaceSpec(newFromSpec); err != nil {
			return nil, false, err
		}
		return distinctNode, true, nil
	}

	return distinctNode, false, nil
}

type MergeFromGroupRule struct {
}

func (MergeFromGroupRule) Name() string {
	return "MergeFromGroupRule"
}

func (MergeFromGroupRule) Pattern() plan.Pattern {
	return plan.Pat(universe.GroupKind, plan.Pat(PhysicalFromKind))
}

func (MergeFromGroupRule) Rewrite(groupNode plan.Node) (plan.Node, bool, error) {
	fromNode := groupNode.Predecessors()[0]
	groupSpec := groupNode.ProcedureSpec().(*universe.GroupProcedureSpec)
	fromSpec := fromNode.ProcedureSpec().(*PhysicalFromProcedureSpec)

	if fromSpec.GroupingSet ||
		fromSpec.LimitSet ||
		groupSpec.GroupMode != flux.GroupModeBy {
		return groupNode, false, nil
	}

	for _, c := range groupSpec.GroupKeys {
		// Storage can only do grouping over tag keys.
		// Note: _start and _stop are okay, since storage is always implicitly grouping by them anyway.
		if c == execute.DefaultTimeColLabel || c == execute.DefaultValueColLabel {
			return groupNode, false, nil
		}
	}

	newFromSpec := fromSpec.Copy().(*PhysicalFromProcedureSpec)
	newFromSpec.GroupingSet = true
	newFromSpec.GroupMode = groupSpec.GroupMode
	newFromSpec.GroupKeys = groupSpec.GroupKeys
	merged, err := plan.MergeToPhysicalNode(groupNode, fromNode, newFromSpec)
	if err != nil {
		return nil, false, err
	}
	return merged, true, nil
}

type FromKeysRule struct {
}

func (FromKeysRule) Name() string {
	return "FromKeysRule"
}

func (FromKeysRule) Pattern() plan.Pattern {
	return plan.Pat(universe.KeysKind, plan.Pat(PhysicalFromKind))
}

func (FromKeysRule) Rewrite(keysNode plan.Node) (plan.Node, bool, error) {
	fromNode := keysNode.Predecessors()[0]
	fromSpec := fromNode.ProcedureSpec().(*PhysicalFromProcedureSpec)

	if fromSpec.LimitSet && fromSpec.PointsLimit == -1 {
		return keysNode, false, nil
	}

	newFromSpec := fromSpec.Copy().(*PhysicalFromProcedureSpec)
	newFromSpec.LimitSet = true
	newFromSpec.PointsLimit = -1

	if err := fromNode.ReplaceSpec(newFromSpec); err != nil {
		return nil, false, err
	}

	return keysNode, true, nil
}

func createFromSource(prSpec plan.ProcedureSpec, dsid execute.DatasetID, a execute.Administration) (execute.Source, error) {
	spec := prSpec.(*PhysicalFromProcedureSpec)
	var w execute.Window
	bounds := a.StreamContext().Bounds()
	if bounds == nil {
		return nil, errors.New("nil bounds passed to from")
	}

	// Note: currently no planner rules will push a window() into from()
	// so the following is dead code.
	if spec.WindowSet {
		w = execute.Window{
			Every:  execute.Duration(spec.Window.Every),
			Period: execute.Duration(spec.Window.Period),
			Offset: execute.Duration(spec.Window.Offset),
		}
	} else {
		duration := execute.Duration(bounds.Stop) - execute.Duration(bounds.Start)
		w = execute.Window{
			Every:  duration,
			Period: duration,
			Offset: bounds.Start.Remainder(duration),
		}
	}
	currentTime := bounds.Start + execute.Time(w.Period)

	deps := a.Dependencies()[FromKind].(Dependencies)

	if len(spec.BucketID) != 0 {
		return nil, errors.New("cannot refer to buckets by their id in 1.x")
	}

	var db, rp string
	if i := strings.IndexByte(spec.Bucket, '/'); i == -1 {
		db = spec.Bucket
	} else {
		rp = spec.Bucket[i+1:]
		db = spec.Bucket[:i]
	}

	// validate and resolve db/rp
	di := deps.MetaClient.Database(db)
	if di == nil {
		return nil, errors.New("no database")
	}

	if deps.AuthEnabled {
		user := meta.UserFromContext(a.Context())
		if user == nil {
			return nil, errors.New("createFromSource: no user")
		}
		if err := deps.Authorizer.AuthorizeDatabase(user, influxql.ReadPrivilege, db); err != nil {
			return nil, err
		}
	}

	if rp == "" {
		rp = di.DefaultRetentionPolicy
	}

	if rpi := di.RetentionPolicy(rp); rpi == nil {
		return nil, errors.New("invalid retention policy")
	}

	return NewSource(
		dsid,
		deps.Reader,
		ReadSpec{
			Database:        db,
			RetentionPolicy: rp,
			Predicate:       spec.Filter,
			PointsLimit:     spec.PointsLimit,
			SeriesLimit:     spec.SeriesLimit,
			SeriesOffset:    spec.SeriesOffset,
			Descending:      spec.Descending,
			OrderByTime:     spec.OrderByTime,
			GroupMode:       ToGroupMode(spec.GroupMode),
			GroupKeys:       spec.GroupKeys,
			AggregateMethod: spec.AggregateMethod,
		},
		*bounds,
		w,
		currentTime,
		a.Allocator(),
	), nil
}

type Authorizer interface {
	AuthorizeDatabase(u meta.User, priv influxql.Privilege, database string) error
}

type Dependencies struct {
	Reader      Reader
	MetaClient  MetaClient
	Authorizer  Authorizer
	AuthEnabled bool
}

func (d Dependencies) Validate() error {
	if d.Reader == nil {
		return errors.New("missing reader dependency")
	}
	if d.MetaClient == nil {
		return errors.New("missing meta client dependency")
	}
	if d.AuthEnabled && d.Authorizer == nil {
		return errors.New("validate Dependencies: missing Authorizer")
	}
	return nil
}

func InjectFromDependencies(depsMap execute.Dependencies, deps Dependencies) error {
	if err := deps.Validate(); err != nil {
		return err
	}
	depsMap[FromKind] = deps
	return nil
}
