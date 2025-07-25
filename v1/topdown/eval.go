package topdown

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/metrics"
	"github.com/open-policy-agent/opa/v1/storage"
	"github.com/open-policy-agent/opa/v1/topdown/builtins"
	"github.com/open-policy-agent/opa/v1/topdown/cache"
	"github.com/open-policy-agent/opa/v1/topdown/copypropagation"
	"github.com/open-policy-agent/opa/v1/topdown/print"
	"github.com/open-policy-agent/opa/v1/tracing"
	"github.com/open-policy-agent/opa/v1/types"
	"github.com/open-policy-agent/opa/v1/util"
)

type evalIterator func(*eval) error

type unifyIterator func() error

type unifyRefIterator func(pos int) error

type queryIDFactory struct {
	curr uint64
}

// Note: The first call to Next() returns 0.
func (f *queryIDFactory) Next() uint64 {
	curr := f.curr
	f.curr++
	return curr
}

type builtinErrors struct {
	errs []error
}

// earlyExitError is used to abort iteration where early exit is possible
type earlyExitError struct {
	prev error
	e    *eval
}

func (ee *earlyExitError) Error() string {
	return fmt.Sprintf("%v: early exit", ee.e.query)
}

type deferredEarlyExitError earlyExitError

func (ee deferredEarlyExitError) Error() string {
	return fmt.Sprintf("%v: deferred early exit", ee.e.query)
}

// Note(æ): this struct is formatted for optimal alignment as it is big, internal and instantiated
// *very* frequently during evaluation. If you need to add fields here, please consider the alignment
// of the struct, and use something like betteralign (https://github.com/dkorunic/betteralign) if you
// need help with that.
type eval struct {
	ctx                         context.Context
	metrics                     metrics.Metrics
	seed                        io.Reader
	cancel                      Cancel
	queryCompiler               ast.QueryCompiler
	store                       storage.Store
	txn                         storage.Transaction
	virtualCache                VirtualCache
	baseCache                   BaseCache
	interQueryBuiltinCache      cache.InterQueryCache
	interQueryBuiltinValueCache cache.InterQueryValueCache
	printHook                   print.Hook
	time                        *ast.Term
	queryIDFact                 *queryIDFactory
	parent                      *eval
	caller                      *eval
	bindings                    *bindings
	compiler                    *ast.Compiler
	input                       *ast.Term
	data                        *ast.Term
	external                    *resolverTrie
	targetStack                 *refStack
	traceLastLocation           *ast.Location // Last location of a trace event.
	instr                       *Instrumentation
	builtins                    map[string]*Builtin
	builtinCache                builtins.Cache
	ndBuiltinCache              builtins.NDBCache
	functionMocks               *functionMocksStack
	comprehensionCache          *comprehensionCache
	saveSet                     *saveSet
	saveStack                   *saveStack
	saveSupport                 *saveSupport
	saveNamespace               *ast.Term
	inliningControl             *inliningControl
	runtime                     *ast.Term
	builtinErrors               *builtinErrors
	roundTripper                CustomizeRoundTripper
	genvarprefix                string
	query                       ast.Body
	tracers                     []QueryTracer
	tracingOpts                 tracing.Options
	queryID                     uint64
	index                       int
	genvarid                    int
	indexing                    bool
	earlyExit                   bool
	traceEnabled                bool
	plugTraceVars               bool
	skipSaveNamespace           bool
	findOne                     bool
	strictObjects               bool
	defined                     bool
}

type evp struct {
	pool sync.Pool
}

func (ep *evp) Put(e *eval) {
	ep.pool.Put(e)
}

func (ep *evp) Get() *eval {
	return ep.pool.Get().(*eval)
}

var evalPool = evp{
	pool: sync.Pool{
		New: func() any {
			return &eval{}
		},
	},
}

func (e *eval) Run(iter evalIterator) error {
	if !e.traceEnabled {
		// avoid function literal escaping to heap if we don't need the trace
		return e.eval(iter)
	}

	e.traceEnter(e.query)
	return e.eval(func(e *eval) error {
		e.traceExit(e.query)
		err := iter(e)
		e.traceRedo(e.query)
		return err
	})
}

func (e *eval) String() string {
	s := strings.Builder{}
	e.string(&s)
	return s.String()
}

func (e *eval) string(s *strings.Builder) {
	fmt.Fprintf(s, "<query: %v index: %d findOne: %v", e.query, e.index, e.findOne)
	if e.parent != nil {
		s.WriteByte(' ')
		e.parent.string(s)
	}
	s.WriteByte('>')
}

func (e *eval) builtinFunc(name string) (*ast.Builtin, BuiltinFunc, bool) {
	decl, ok := ast.BuiltinMap[name]
	if ok {
		f, ok := builtinFunctions[name]
		if ok {
			return decl, f, true
		}
	} else {
		bi, ok := e.builtins[name]
		if ok {
			return bi.Decl, bi.Func, true
		}
	}
	return nil, nil, false
}

func (e *eval) closure(query ast.Body, cpy *eval) {
	*cpy = *e
	cpy.index = 0
	cpy.query = query
	cpy.queryID = cpy.queryIDFact.Next()
	cpy.parent = e
	cpy.findOne = false
}

func (e *eval) child(query ast.Body, cpy *eval) {
	*cpy = *e
	cpy.index = 0
	cpy.query = query
	cpy.queryID = cpy.queryIDFact.Next()
	cpy.bindings = newBindings(cpy.queryID, e.instr)
	cpy.parent = e
	cpy.findOne = false
}

func (e *eval) next(iter evalIterator) error {
	e.index++
	err := e.evalExpr(iter)
	e.index--
	return err
}

func (e *eval) partial() bool {
	return e.saveSet != nil
}

func (e *eval) unknown(x any, b *bindings) bool {
	if !e.partial() {
		return false
	}

	// If the caller provided an ast.Value directly (e.g., an ast.Ref) wrap
	// it as an ast.Term because the saveSet Contains() function expects
	// ast.Term.
	if v, ok := x.(ast.Value); ok {
		x = ast.NewTerm(v)
	}

	return saveRequired(e.compiler, e.inliningControl, true, e.saveSet, b, x, false)
}

// exactly like `unknown` above` but without the cost of `any` boxing when arg is known to be a ref
func (e *eval) unknownRef(ref ast.Ref, b *bindings) bool {
	return e.partial() && saveRequired(e.compiler, e.inliningControl, true, e.saveSet, b, ast.NewTerm(ref), false)
}

func (e *eval) traceEnter(x ast.Node) {
	e.traceEvent(EnterOp, x, "", nil)
}

func (e *eval) traceExit(x ast.Node) {
	var msg string
	if e.findOne {
		msg = "early"
	}
	e.traceEvent(ExitOp, x, msg, nil)
}

func (e *eval) traceEval(x ast.Node) {
	e.traceEvent(EvalOp, x, "", nil)
}

func (e *eval) traceDuplicate(x ast.Node) {
	e.traceEvent(DuplicateOp, x, "", nil)
}

func (e *eval) traceFail(x ast.Node) {
	e.traceEvent(FailOp, x, "", nil)
}

func (e *eval) traceRedo(x ast.Node) {
	e.traceEvent(RedoOp, x, "", nil)
}

func (e *eval) traceSave(x ast.Node) {
	e.traceEvent(SaveOp, x, "", nil)
}

func (e *eval) traceIndex(x ast.Node, msg string, target *ast.Ref) {
	e.traceEvent(IndexOp, x, msg, target)
}

func (e *eval) traceWasm(x ast.Node, target *ast.Ref) {
	e.traceEvent(WasmOp, x, "", target)
}

func (e *eval) traceUnify(a, b *ast.Term) {
	e.traceEvent(UnifyOp, ast.Equality.Expr(a, b), "", nil)
}

func (e *eval) traceEvent(op Op, x ast.Node, msg string, target *ast.Ref) {

	if !e.traceEnabled {
		return
	}

	var parentID uint64
	if e.parent != nil {
		parentID = e.parent.queryID
	}

	location := x.Loc()
	if location == nil {
		location = e.traceLastLocation
	} else {
		e.traceLastLocation = location
	}

	evt := Event{
		QueryID:  e.queryID,
		ParentID: parentID,
		Op:       op,
		Node:     x,
		Location: location,
		Message:  msg,
		Ref:      target,
		input:    e.input,
		bindings: e.bindings,
	}

	// Skip plugging the local variables, unless any of the tracers
	// had required it via their configuration. If any required the
	// variable bindings then we will plug and give values for all
	// tracers.
	if e.plugTraceVars {

		evt.Locals = ast.NewValueMap()
		evt.LocalMetadata = map[ast.Var]VarMetadata{}
		evt.localVirtualCacheSnapshot = ast.NewValueMap()

		_ = e.bindings.Iter(nil, func(k, v *ast.Term) error {
			original := k.Value.(ast.Var)
			rewritten, _ := e.rewrittenVar(original)
			evt.LocalMetadata[original] = VarMetadata{
				Name:     rewritten,
				Location: k.Loc(),
			}

			// For backwards compatibility save a copy of the values too..
			evt.Locals.Put(k.Value, v.Value)
			return nil
		}) // cannot return error

		ast.WalkTerms(x, func(term *ast.Term) bool {
			switch x := term.Value.(type) {
			case ast.Var:
				if _, ok := evt.LocalMetadata[x]; !ok {
					if rewritten, ok := e.rewrittenVar(x); ok {
						evt.LocalMetadata[x] = VarMetadata{
							Name:     rewritten,
							Location: term.Loc(),
						}
					}
				}
			case ast.Ref:
				groundRef := x.GroundPrefix()
				if v, _ := e.virtualCache.Get(groundRef); v != nil {
					evt.localVirtualCacheSnapshot.Put(groundRef, v.Value)
				}
			}
			return false
		})
	}

	for i := range e.tracers {
		e.tracers[i].TraceEvent(evt)
	}
}

func (e *eval) eval(iter evalIterator) error {
	return e.evalExpr(iter)
}

func (e *eval) evalExpr(iter evalIterator) error {
	wrapErr := func(err error) error {
		if !e.findOne {
			// The current rule/function doesn't support EE, but a caller (somewhere down the call stack) does.
			return &deferredEarlyExitError{prev: err, e: e}
		}
		return &earlyExitError{prev: err, e: e}
	}

	if e.cancel != nil && e.cancel.Cancelled() {
		if e.ctx != nil && e.ctx.Err() != nil {
			return &Error{
				Code:    CancelErr,
				Message: e.ctx.Err().Error(),
				err:     e.ctx.Err(),
			}
		}
		return &Error{
			Code:    CancelErr,
			Message: "caller cancelled query execution",
		}
	}

	if e.index >= len(e.query) {
		if err := iter(e); err != nil {
			switch err := err.(type) {
			case *deferredEarlyExitError, *earlyExitError:
				return wrapErr(err)
			default:
				return err
			}
		}

		if e.findOne && !e.partial() { // we've found one!
			return &earlyExitError{e: e}
		}
		return nil
	}
	expr := e.query[e.index]

	e.traceEval(expr)

	if len(expr.With) > 0 {
		return e.evalWith(iter)
	}

	return e.evalStep(func(e *eval) error {
		return e.next(iter)
	})
}

func (e *eval) evalStep(iter evalIterator) error {
	expr := e.query[e.index]

	if expr.Negated {
		return e.evalNot(iter)
	}

	var err error

	// NOTE(æ): the reason why there's one branch for the tracing case and one almost
	// identical branch below for when tracing is disabled is that the tracing case
	// allocates wildly. These allocations are cause by the "defined" boolean variable
	// escaping to the heap as its value is set from inside of closures. There may very
	// well be more elegant solutions to this problem, but this is one that works, and
	// saves several *million* allocations for some workloads. So feel free to refactor
	// this, but do make sure that the common non-tracing case doesn't pay in allocations
	// for something that is only needed when tracing is enabled.
	if e.traceEnabled {
		var defined bool
		switch terms := expr.Terms.(type) {
		case []*ast.Term:
			switch {
			case expr.IsEquality():
				err = e.unify(terms[1], terms[2], func() error {
					defined = true
					err := iter(e)
					e.traceRedo(expr)
					return err
				})
			default:
				err = e.evalCall(terms, func() error {
					defined = true
					err := iter(e)
					e.traceRedo(expr)
					return err
				})
			}
		case *ast.Term:
			// generateVar inlined here to avoid extra allocations in hot path
			rterm := ast.VarTerm(e.fmtVarTerm())

			if e.partial() {
				e.inliningControl.PushDisable(rterm.Value, true)
			}

			err = e.unify(terms, rterm, func() error {
				if e.saveSet.Contains(rterm, e.bindings) {
					return e.saveExpr(ast.NewExpr(rterm), e.bindings, func() error {
						return iter(e)
					})
				}
				if !e.bindings.Plug(rterm).Equal(ast.InternedTerm(false)) {
					defined = true
					err := iter(e)
					e.traceRedo(expr)
					return err
				}
				return nil
			})

			if e.partial() {
				e.inliningControl.PopDisable()
			}
		case *ast.Every:
			eval := evalEvery{
				Every: terms,
				e:     e,
				expr:  expr,
			}
			err = eval.eval(func() error {
				defined = true
				err := iter(e)
				e.traceRedo(expr)
				return err
			})

		default: // guard-rail for adding extra (Expr).Terms types
			return fmt.Errorf("got %T terms: %[1]v", terms)
		}

		if err != nil {
			return err
		}

		if !defined {
			e.traceFail(expr)
		}

		return nil
	}

	switch terms := expr.Terms.(type) {
	case []*ast.Term:
		switch {
		case expr.IsEquality():
			err = e.unify(terms[1], terms[2], func() error {
				return iter(e)
			})
		default:
			err = e.evalCall(terms, func() error {
				return iter(e)
			})
		}
	case *ast.Term:
		// generateVar inlined here to avoid extra allocations in hot path
		rterm := ast.VarTerm(e.fmtVarTerm())
		err = e.unify(terms, rterm, func() error {
			if e.saveSet.Contains(rterm, e.bindings) {
				return e.saveExpr(ast.NewExpr(rterm), e.bindings, func() error {
					return iter(e)
				})
			}
			if !e.bindings.Plug(rterm).Equal(ast.InternedTerm(false)) {
				return iter(e)
			}
			return nil
		})
	case *ast.Every:
		eval := evalEvery{
			Every: terms,
			e:     e,
			expr:  expr,
		}
		err = eval.eval(func() error {
			return iter(e)
		})

	default: // guard-rail for adding extra (Expr).Terms types
		return fmt.Errorf("got %T terms: %[1]v", terms)
	}

	return err
}

// Single-purpose fmt.Sprintf replacement for generating variable names with only
// one allocation performed instead of 4, and in 1/3 the time.
func (e *eval) fmtVarTerm() string {
	buf := make([]byte, 0, len(e.genvarprefix)+util.NumDigitsUint(e.queryID)+util.NumDigitsInt(e.index)+7)

	buf = append(buf, e.genvarprefix...)
	buf = append(buf, "_term_"...)
	buf = strconv.AppendUint(buf, e.queryID, 10)
	buf = append(buf, '_')
	buf = strconv.AppendInt(buf, int64(e.index), 10)

	return util.ByteSliceToString(buf)
}

func (e *eval) evalNot(iter evalIterator) error {
	expr := e.query[e.index]

	if e.unknown(expr, e.bindings) {
		return e.evalNotPartial(iter)
	}

	negation := ast.NewBody(expr.ComplementNoWith())
	child := evalPool.Get()
	defer evalPool.Put(child)

	e.closure(negation, child)

	if e.traceEnabled {
		child.traceEnter(negation)
	}

	if err := child.eval(func(*eval) error {
		if e.traceEnabled {
			child.traceExit(negation)
			child.traceRedo(negation)
		}
		child.defined = true

		return nil
	}); err != nil {
		return err
	}

	if !child.defined {
		return iter(e)
	}

	child.defined = false

	e.traceFail(expr)
	return nil
}

func (e *eval) evalWith(iter evalIterator) error {

	expr := e.query[e.index]

	var disable []ast.Ref

	if e.partial() {
		// Avoid the `disable` var to escape to heap unless partial evaluation is enabled.
		var disablePartial []ast.Ref
		// Disable inlining on all references in the expression so the result of
		// partial evaluation has the same semantics w/ the with statements
		// preserved.
		disableRef := func(x ast.Ref) bool {
			disablePartial = append(disablePartial, x.GroundPrefix())
			return false
		}

		// If the value is unknown the with statement cannot be evaluated and so
		// the entire expression should be saved to be safe. In the future this
		// could be relaxed in certain cases (e.g., if the with statement would
		// have no effect.)
		for _, with := range expr.With {
			if isFunction(e.compiler.TypeEnv, with.Target) || // non-builtin function replaced
				isOtherRef(with.Target) { // built-in replaced

				ast.WalkRefs(with.Value, disableRef)
				continue
			}

			// with target is data or input (not built-in)
			if e.saveSet.ContainsRecursive(with.Value, e.bindings) {
				return e.saveExprMarkUnknowns(expr, e.bindings, func() error {
					return e.next(iter)
				})
			}
			ast.WalkRefs(with.Target, disableRef)
			ast.WalkRefs(with.Value, disableRef)
		}

		ast.WalkRefs(expr.NoWith(), disableRef)

		disable = disablePartial
	}

	pairsInput := [][2]*ast.Term{}
	pairsData := [][2]*ast.Term{}
	targets := make([]ast.Ref, 0, len(expr.With))

	var functionMocks [][2]*ast.Term

	for i := range expr.With {
		target := expr.With[i].Target
		plugged := e.bindings.Plug(expr.With[i].Value)
		switch {
		// NOTE(sr): ordering matters here: isFunction's ref is also covered by isDataRef
		case isFunction(e.compiler.TypeEnv, target):
			functionMocks = append(functionMocks, [...]*ast.Term{target, plugged})

		case isInputRef(target):
			pairsInput = append(pairsInput, [...]*ast.Term{target, plugged})

		case isDataRef(target):
			pairsData = append(pairsData, [...]*ast.Term{target, plugged})

		default: // target must be builtin
			if _, _, ok := e.builtinFunc(target.String()); ok {
				functionMocks = append(functionMocks, [...]*ast.Term{target, plugged})
				continue // don't append to disabled targets below
			}
		}
		targets = append(targets, target.Value.(ast.Ref))
	}

	input, err := mergeTermWithValues(e.input, pairsInput)
	if err != nil {
		return &Error{
			Code:     ConflictErr,
			Location: expr.Location,
			Message:  err.Error(),
		}
	}

	data, err := mergeTermWithValues(e.data, pairsData)
	if err != nil {
		return &Error{
			Code:     ConflictErr,
			Location: expr.Location,
			Message:  err.Error(),
		}
	}

	oldInput, oldData := e.evalWithPush(input, data, functionMocks, targets, disable)

	err = e.evalStep(func(e *eval) error {
		e.evalWithPop(oldInput, oldData)
		err := e.next(iter)
		oldInput, oldData = e.evalWithPush(input, data, functionMocks, targets, disable)
		return err
	})

	e.evalWithPop(oldInput, oldData)

	return err
}

func (e *eval) evalWithPush(input, data *ast.Term, functionMocks [][2]*ast.Term, targets, disable []ast.Ref) (*ast.Term, *ast.Term) {
	var oldInput *ast.Term

	if input != nil {
		oldInput = e.input
		e.input = input
	}

	var oldData *ast.Term

	if data != nil {
		oldData = e.data
		e.data = data
	}

	if e.comprehensionCache == nil {
		e.comprehensionCache = newComprehensionCache()
	}

	e.comprehensionCache.Push()
	e.virtualCache.Push()

	if e.targetStack == nil {
		e.targetStack = newRefStack()
	}

	e.targetStack.Push(targets)
	e.inliningControl.PushDisable(disable, true)

	if e.functionMocks == nil {
		e.functionMocks = newFunctionMocksStack()
	}

	e.functionMocks.PutPairs(functionMocks)

	return oldInput, oldData
}

func (e *eval) evalWithPop(input, data *ast.Term) {
	// NOTE(ae) no nil checks here as we assume evalWithPush always called first
	e.inliningControl.PopDisable()
	e.targetStack.Pop()
	e.virtualCache.Pop()
	e.comprehensionCache.Pop()
	e.functionMocks.PopPairs()
	e.data = data
	e.input = input
}

func (e *eval) evalNotPartial(iter evalIterator) error {
	// Prepare query normally.
	expr := e.query[e.index]
	negation := expr.ComplementNoWith()

	child := evalPool.Get()
	defer evalPool.Put(child)

	e.closure(ast.NewBody(negation), child)

	// Unknowns is the set of variables that are marked as unknown. The variables
	// are namespaced with the query ID that they originate in. This ensures that
	// variables across two or more queries are identified uniquely.
	//
	// NOTE(tsandall): this is greedy in the sense that we only need variable
	// dependencies of the negation.
	unknowns := e.saveSet.Vars(e.caller.bindings)

	// Run partial evaluation. Since the result may require support, push a new
	// query onto the save stack to avoid mutating the current save query. If
	// shallow inlining is not enabled, run copy propagation to further simplify
	// the result.
	var cp *copypropagation.CopyPropagator

	if !e.inliningControl.shallow {
		cp = copypropagation.New(unknowns).WithEnsureNonEmptyBody(true).WithCompiler(e.compiler)
	}

	var savedQueries []ast.Body
	e.saveStack.PushQuery(nil)

	_ = child.eval(func(*eval) error {
		query := e.saveStack.Peek()
		plugged := query.Plug(e.caller.bindings)
		// Skip this rule body if it fails to type-check.
		// Type-checking failure means the rule body will never succeed.
		if !e.compiler.PassesTypeCheck(plugged) {
			return nil
		}
		if cp != nil {
			plugged = applyCopyPropagation(cp, e.instr, plugged)
		}
		savedQueries = append(savedQueries, plugged)
		return nil
	}) // cannot return error

	e.saveStack.PopQuery()

	// If partial evaluation produced no results, the expression is always undefined
	// so it does not have to be saved.
	if len(savedQueries) == 0 {
		return iter(e)
	}

	// Check if the partial evaluation result can be inlined in this query. If not,
	// generate support rules for the result. Depending on the size of the partial
	// evaluation result and the contents, it may or may not be inlinable. We treat
	// the unknowns as safe because vars in the save set will either be known to
	// the caller or made safe by an expression on the save stack.
	if !canInlineNegation(unknowns, savedQueries) {
		return e.evalNotPartialSupport(child.queryID, expr, unknowns, savedQueries, iter)
	}

	// If we can inline the result, we have to generate the cross product of the
	// queries. For example:
	//
	//	(A && B) || (C && D)
	//
	// Becomes:
	//
	//	(!A && !C) || (!A && !D) || (!B && !C) || (!B && !D)
	return complementedCartesianProduct(savedQueries, 0, nil, func(q ast.Body) error {
		return e.saveInlinedNegatedExprs(q, func() error {
			return iter(e)
		})
	})
}

func (e *eval) evalNotPartialSupport(negationID uint64, expr *ast.Expr, unknowns ast.VarSet, queries []ast.Body, iter evalIterator) error {

	// Prepare support rule head.
	supportName := fmt.Sprintf("__not%d_%d_%d__", e.queryID, e.index, negationID)
	term := ast.RefTerm(ast.DefaultRootDocument, e.saveNamespace, ast.StringTerm(supportName))
	path := term.Value.(ast.Ref)
	head := ast.NewHead(ast.Var(supportName), nil, ast.BooleanTerm(true))

	bodyVars := ast.NewVarSet()

	for _, q := range queries {
		bodyVars.Update(q.Vars(ast.VarVisitorParams{}))
	}

	unknowns = unknowns.Intersect(bodyVars)

	// Make rule args. Sort them to ensure order is deterministic.
	args := make([]*ast.Term, 0, len(unknowns))

	for v := range unknowns {
		args = append(args, ast.NewTerm(v))
	}

	slices.SortFunc(args, ast.TermValueCompare)

	if len(args) > 0 {
		head.Args = args
	}

	// Save support rules.
	for _, query := range queries {
		e.saveSupport.Insert(path, &ast.Rule{
			Head: head,
			Body: query,
		})
	}

	// Save expression that refers to support rule set.
	cpy := expr.CopyWithoutTerms()

	if len(args) > 0 {
		terms := make([]*ast.Term, len(args)+1)
		terms[0] = term
		copy(terms[1:], args)
		cpy.Terms = terms
	} else {
		cpy.Terms = term
	}

	return e.saveInlinedNegatedExprs([]*ast.Expr{cpy}, func() error {
		return e.next(iter)
	})
}

func (e *eval) evalCall(terms []*ast.Term, iter unifyIterator) error {

	ref := terms[0].Value.(ast.Ref)

	mock, mocked := e.functionMocks.Get(ref)
	if mocked {
		if m, ok := mock.Value.(ast.Ref); ok && isFunction(e.compiler.TypeEnv, m) { // builtin or data function
			mockCall := append([]*ast.Term{ast.NewTerm(m)}, terms[1:]...)

			e.functionMocks.Push()
			err := e.evalCall(mockCall, func() error {
				e.functionMocks.Pop()
				err := iter()
				e.functionMocks.Push()
				return err
			})
			e.functionMocks.Pop()
			return err
		}
	}
	// 'mocked' true now indicates that the replacement is a value: if
	// it was a ref to a function, we'd have called that above.

	if ref[0].Equal(ast.DefaultRootDocument) {
		if mocked {
			f := e.compiler.TypeEnv.Get(ref).(*types.Function)
			return e.evalCallValue(f.Arity(), terms, mock, iter)
		}

		var ir *ast.IndexResult
		var err error
		if e.partial() {
			ir, err = e.getRules(ref, nil)
		} else {
			ir, err = e.getRules(ref, terms[1:])
		}
		defer ast.IndexResultPool.Put(ir)
		if err != nil {
			return err
		}

		eval := evalFunc{
			e:     e,
			terms: terms,
			ir:    ir,
		}
		return eval.eval(iter)
	}

	builtinName := ref.String()
	bi, f, ok := e.builtinFunc(builtinName)
	if !ok {
		return unsupportedBuiltinErr(e.query[e.index].Location)
	}

	if mocked { // value replacement of built-in call
		return e.evalCallValue(bi.Decl.Arity(), terms, mock, iter)
	}

	if e.unknown(e.query[e.index], e.bindings) {
		return e.saveCall(bi.Decl.Arity(), terms, iter)
	}

	var bctx *BuiltinContext

	// Creating a BuiltinContext is expensive, so only do it if the builtin depends on it.
	if bi.NeedsBuiltInContext() {
		var parentID uint64
		if e.parent != nil {
			parentID = e.parent.queryID
		}

		var capabilities *ast.Capabilities
		if e.compiler != nil {
			capabilities = e.compiler.Capabilities()
		}

		bctx = &BuiltinContext{
			Context:                     e.ctx,
			Metrics:                     e.metrics,
			Seed:                        e.seed,
			Time:                        e.time,
			Cancel:                      e.cancel,
			Runtime:                     e.runtime,
			Cache:                       e.builtinCache,
			InterQueryBuiltinCache:      e.interQueryBuiltinCache,
			InterQueryBuiltinValueCache: e.interQueryBuiltinValueCache,
			NDBuiltinCache:              e.ndBuiltinCache,
			Location:                    e.query[e.index].Location,
			QueryTracers:                e.tracers,
			TraceEnabled:                e.traceEnabled,
			QueryID:                     e.queryID,
			ParentID:                    parentID,
			PrintHook:                   e.printHook,
			DistributedTracingOpts:      e.tracingOpts,
			Capabilities:                capabilities,
			RoundTripper:                e.roundTripper,
		}
	}

	eval := evalBuiltin{
		e:     e,
		bi:    bi,
		bctx:  bctx,
		f:     f,
		terms: terms[1:],
	}

	return eval.eval(iter)
}

func (e *eval) evalCallValue(arity int, terms []*ast.Term, mock *ast.Term, iter unifyIterator) error {
	switch {
	case len(terms) == arity+2: // captured var
		return e.unify(terms[len(terms)-1], mock, iter)

	case len(terms) == arity+1:
		if !ast.Boolean(false).Equal(mock.Value) {
			return iter()
		}
		return nil
	}
	panic("unreachable")
}

func (e *eval) unify(a, b *ast.Term, iter unifyIterator) error {
	return e.biunify(a, b, e.bindings, e.bindings, iter)
}

func (e *eval) biunify(a, b *ast.Term, b1, b2 *bindings, iter unifyIterator) error {
	a, b1 = b1.apply(a)
	b, b2 = b2.apply(b)
	if e.traceEnabled {
		e.traceUnify(a, b)
	}
	switch vA := a.Value.(type) {
	case ast.Var, ast.Ref, *ast.ArrayComprehension, *ast.SetComprehension, *ast.ObjectComprehension:
		return e.biunifyValues(a, b, b1, b2, iter)
	case ast.Null:
		switch b.Value.(type) {
		case ast.Var, ast.Null, ast.Ref:
			return e.biunifyValues(a, b, b1, b2, iter)
		}
	case ast.Boolean:
		switch b.Value.(type) {
		case ast.Var, ast.Boolean, ast.Ref:
			return e.biunifyValues(a, b, b1, b2, iter)
		}
	case ast.Number:
		switch b.Value.(type) {
		case ast.Var, ast.Number, ast.Ref:
			return e.biunifyValues(a, b, b1, b2, iter)
		}
	case ast.String:
		switch b.Value.(type) {
		case ast.Var, ast.String, ast.Ref:
			return e.biunifyValues(a, b, b1, b2, iter)
		}
	case *ast.Array:
		switch vB := b.Value.(type) {
		case ast.Var, ast.Ref, *ast.ArrayComprehension:
			return e.biunifyValues(a, b, b1, b2, iter)
		case *ast.Array:
			return e.biunifyArrays(vA, vB, b1, b2, iter)
		}
	case ast.Object:
		switch vB := b.Value.(type) {
		case ast.Var, ast.Ref, *ast.ObjectComprehension:
			return e.biunifyValues(a, b, b1, b2, iter)
		case ast.Object:
			return e.biunifyObjects(vA, vB, b1, b2, iter)
		}
	case ast.Set:
		return e.biunifyValues(a, b, b1, b2, iter)
	}
	return nil
}

func (e *eval) biunifyArrays(a, b *ast.Array, b1, b2 *bindings, iter unifyIterator) error {
	if a.Len() != b.Len() {
		return nil
	}
	return e.biunifyArraysRec(a, b, b1, b2, iter, 0)
}

func (e *eval) biunifyArraysRec(a, b *ast.Array, b1, b2 *bindings, iter unifyIterator, idx int) error {
	if idx == a.Len() {
		return iter()
	}
	return e.biunify(a.Elem(idx), b.Elem(idx), b1, b2, func() error {
		return e.biunifyArraysRec(a, b, b1, b2, iter, idx+1)
	})
}

func (e *eval) biunifyTerms(a, b []*ast.Term, b1, b2 *bindings, iter unifyIterator) error {
	if len(a) != len(b) {
		return nil
	}
	return e.biunifyTermsRec(a, b, b1, b2, iter, 0)
}

func (e *eval) biunifyTermsRec(a, b []*ast.Term, b1, b2 *bindings, iter unifyIterator, idx int) error {
	if idx == len(a) {
		return iter()
	}
	return e.biunify(a[idx], b[idx], b1, b2, func() error {
		return e.biunifyTermsRec(a, b, b1, b2, iter, idx+1)
	})
}

func (e *eval) biunifyObjects(a, b ast.Object, b1, b2 *bindings, iter unifyIterator) error {
	if a.Len() != b.Len() {
		return nil
	}

	// Objects must not contain unbound variables as keys at this point as we
	// cannot unify them. Similar to sets, plug both sides before comparing the
	// keys and unifying the values.
	if nonGroundKeys(a) {
		a = plugKeys(a, b1)
	}

	if nonGroundKeys(b) {
		b = plugKeys(b, b2)
	}

	return e.biunifyObjectsRec(a, b, b1, b2, iter, a, a.KeysIterator())
}

func (e *eval) biunifyObjectsRec(a, b ast.Object, b1, b2 *bindings, iter unifyIterator, keys ast.Object, oki ast.ObjectKeysIterator) error {
	key, more := oki.Next() // Get next key from iterator.
	if !more {
		return iter()
	}
	v2 := b.Get(key)
	if v2 == nil {
		return nil
	}
	return e.biunify(a.Get(key), v2, b1, b2, func() error {
		return e.biunifyObjectsRec(a, b, b1, b2, iter, keys, oki)
	})
}

func (e *eval) biunifyValues(a, b *ast.Term, b1, b2 *bindings, iter unifyIterator) error {
	// Try to evaluate refs and comprehensions. If partial evaluation is
	// enabled, then skip evaluation (and save the expression) if the term is
	// in the save set. Currently, comprehensions are not evaluated during
	// partial eval. This could be improved in the future.

	var saveA, saveB bool

	if _, ok := a.Value.(ast.Set); ok {
		saveA = e.saveSet.ContainsRecursive(a, b1)
	} else {
		saveA = e.saveSet.Contains(a, b1)
		if !saveA {
			if _, refA := a.Value.(ast.Ref); refA {
				return e.biunifyRef(a, b, b1, b2, iter)
			}
		}
	}

	if _, ok := b.Value.(ast.Set); ok {
		saveB = e.saveSet.ContainsRecursive(b, b2)
	} else {
		saveB = e.saveSet.Contains(b, b2)
		if !saveB {
			if _, refB := b.Value.(ast.Ref); refB {
				return e.biunifyRef(b, a, b2, b1, iter)
			}
		}
	}

	if saveA || saveB {
		return e.saveUnify(a, b, b1, b2, iter)
	}

	if ast.IsComprehension(a.Value) {
		return e.biunifyComprehension(a, b, b1, b2, false, iter)
	} else if ast.IsComprehension(b.Value) {
		return e.biunifyComprehension(b, a, b2, b1, true, iter)
	}

	// Perform standard unification.
	_, varA := a.Value.(ast.Var)
	_, varB := b.Value.(ast.Var)

	var undo undo

	if varA && varB {
		if b1 == b2 && a.Equal(b) {
			return iter()
		}
		b1.bind(a, b, b2, &undo)
		err := iter()
		undo.Undo()
		return err
	} else if varA && !varB {
		b1.bind(a, b, b2, &undo)
		err := iter()
		undo.Undo()
		return err
	} else if varB && !varA {
		b2.bind(b, a, b1, &undo)
		err := iter()
		undo.Undo()
		return err
	}

	// Sets must not contain unbound variables at this point as we cannot unify
	// them. So simply plug both sides (to substitute any bound variables with
	// values) and then check for equality.
	switch a.Value.(type) {
	case ast.Set:
		a = b1.Plug(a)
		b = b2.Plug(b)
	}

	if a.Equal(b) {
		return iter()
	}

	return nil
}

func (e *eval) biunifyRef(a, b *ast.Term, b1, b2 *bindings, iter unifyIterator) error {

	ref := a.Value.(ast.Ref)

	if ref[0].Equal(ast.DefaultRootDocument) {
		node := e.compiler.RuleTree.Child(ref[0].Value)
		eval := evalTree{
			e:         e,
			ref:       ref,
			pos:       1,
			plugged:   ref.CopyNonGround(),
			bindings:  b1,
			rterm:     b,
			rbindings: b2,
			node:      node,
		}
		return eval.eval(iter)
	}

	var term *ast.Term
	var termbindings *bindings

	if ref[0].Equal(ast.InputRootDocument) {
		term = e.input
		termbindings = b1
	} else {
		term, termbindings = b1.apply(ref[0])
		if term == ref[0] {
			term = nil
		}
	}

	if term == nil {
		return nil
	}

	eval := evalTerm{
		e:            e,
		ref:          ref,
		pos:          1,
		bindings:     b1,
		term:         term,
		termbindings: termbindings,
		rterm:        b,
		rbindings:    b2,
	}

	return eval.eval(iter)
}

func (e *eval) biunifyComprehension(a, b *ast.Term, b1, b2 *bindings, swap bool, iter unifyIterator) error {

	if e.unknown(a, b1) {
		return e.biunifyComprehensionPartial(a, b, b1, b2, swap, iter)
	}

	value, err := e.buildComprehensionCache(a)

	if err != nil {
		return err
	} else if value != nil {
		return e.biunify(value, b, b1, b2, iter)
	}

	e.instr.counterIncr(evalOpComprehensionCacheMiss)

	switch a := a.Value.(type) {
	case *ast.ArrayComprehension:
		return e.biunifyComprehensionArray(a, b, b1, b2, iter)
	case *ast.SetComprehension:
		return e.biunifyComprehensionSet(a, b, b1, b2, iter)
	case *ast.ObjectComprehension:
		return e.biunifyComprehensionObject(a, b, b1, b2, iter)
	}

	return internalErr(e.query[e.index].Location, "illegal comprehension type")
}

func (e *eval) buildComprehensionCache(a *ast.Term) (*ast.Term, error) {

	index := e.comprehensionIndex(a)
	if index == nil {
		e.instr.counterIncr(evalOpComprehensionCacheSkip)
		return nil, nil
	}

	if e.comprehensionCache == nil {
		e.comprehensionCache = newComprehensionCache()
	}

	cache, ok := e.comprehensionCache.Elem(a)
	if !ok {
		var err error
		switch x := a.Value.(type) {
		case *ast.ArrayComprehension:
			cache, err = e.buildComprehensionCacheArray(x, index.Keys)
		case *ast.SetComprehension:
			cache, err = e.buildComprehensionCacheSet(x, index.Keys)
		case *ast.ObjectComprehension:
			cache, err = e.buildComprehensionCacheObject(x, index.Keys)
		default:
			err = internalErr(e.query[e.index].Location, "illegal comprehension type")
		}
		if err != nil {
			return nil, err
		}
		e.comprehensionCache.Set(a, cache)
		e.instr.counterIncr(evalOpComprehensionCacheBuild)
	} else {
		e.instr.counterIncr(evalOpComprehensionCacheHit)
	}

	values := make([]*ast.Term, len(index.Keys))

	for i := range index.Keys {
		values[i] = e.bindings.Plug(index.Keys[i])
	}

	return cache.Get(values), nil
}

func (e *eval) buildComprehensionCacheArray(x *ast.ArrayComprehension, keys []*ast.Term) (*comprehensionCacheElem, error) {
	child := evalPool.Get()
	defer evalPool.Put(child)

	e.child(x.Body, child)
	node := newComprehensionCacheElem()
	return node, child.Run(func(child *eval) error {
		values := make([]*ast.Term, len(keys))
		for i := range keys {
			values[i] = child.bindings.Plug(keys[i])
		}
		head := child.bindings.Plug(x.Term)
		cached := node.Get(values)
		if cached != nil {
			cached.Value = cached.Value.(*ast.Array).Append(head)
		} else {
			node.Put(values, ast.ArrayTerm(head))
		}
		return nil
	})
}

func (e *eval) buildComprehensionCacheSet(x *ast.SetComprehension, keys []*ast.Term) (*comprehensionCacheElem, error) {
	child := evalPool.Get()
	defer evalPool.Put(child)

	e.child(x.Body, child)
	node := newComprehensionCacheElem()
	return node, child.Run(func(child *eval) error {
		values := make([]*ast.Term, len(keys))
		for i := range keys {
			values[i] = child.bindings.Plug(keys[i])
		}
		head := child.bindings.Plug(x.Term)
		cached := node.Get(values)
		if cached != nil {
			set := cached.Value.(ast.Set)
			set.Add(head)
		} else {
			node.Put(values, ast.SetTerm(head))
		}
		return nil
	})
}

func (e *eval) buildComprehensionCacheObject(x *ast.ObjectComprehension, keys []*ast.Term) (*comprehensionCacheElem, error) {
	child := evalPool.Get()
	defer evalPool.Put(child)

	e.child(x.Body, child)
	node := newComprehensionCacheElem()
	return node, child.Run(func(child *eval) error {
		values := make([]*ast.Term, len(keys))
		for i := range keys {
			values[i] = child.bindings.Plug(keys[i])
		}
		headKey := child.bindings.Plug(x.Key)
		headValue := child.bindings.Plug(x.Value)
		cached := node.Get(values)
		if cached != nil {
			obj := cached.Value.(ast.Object)
			obj.Insert(headKey, headValue)
		} else {
			node.Put(values, ast.ObjectTerm(ast.Item(headKey, headValue)))
		}
		return nil
	})
}

func (e *eval) biunifyComprehensionPartial(a, b *ast.Term, b1, b2 *bindings, swap bool, iter unifyIterator) error {
	var err error
	cpyA, err := e.amendComprehension(a, b1)
	if err != nil {
		return err
	}

	if ast.IsComprehension(b.Value) {
		b, err = e.amendComprehension(b, b2)
		if err != nil {
			return err
		}
	}

	// The other term might need to be plugged so include the bindings. The
	// bindings for the comprehension term are saved (for compatibility) but
	// the eventual plug operation on the comprehension will be a no-op.
	if !swap {
		return e.saveUnify(cpyA, b, b1, b2, iter)
	}

	return e.saveUnify(b, cpyA, b2, b1, iter)
}

// amendComprehension captures bindings available to the comprehension,
// and used within its term or body.
func (e *eval) amendComprehension(a *ast.Term, b1 *bindings) (*ast.Term, error) {
	cpyA := a.Copy()

	// Namespace the variables in the body to avoid collision when the final
	// queries returned by partial evaluation.
	var body *ast.Body

	switch a := cpyA.Value.(type) {
	case *ast.ArrayComprehension:
		body = &a.Body
	case *ast.SetComprehension:
		body = &a.Body
	case *ast.ObjectComprehension:
		body = &a.Body
	default:
		return nil, fmt.Errorf("illegal comprehension %T", a)
	}

	vars := a.Vars()
	err := b1.Iter(e.caller.bindings, func(k, v *ast.Term) error {
		if vars.Contains(k.Value.(ast.Var)) {
			body.Append(ast.Equality.Expr(k, v))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	b1.Namespace(cpyA, e.caller.bindings)
	return cpyA, nil
}

func (e *eval) biunifyComprehensionArray(x *ast.ArrayComprehension, b *ast.Term, b1, b2 *bindings, iter unifyIterator) error {
	result := ast.NewArray()
	child := evalPool.Get()

	e.closure(x.Body, child)
	defer evalPool.Put(child)

	err := child.Run(func(child *eval) error {
		result = result.Append(child.bindings.Plug(x.Term))
		return nil
	})
	if err != nil {
		return err
	}
	return e.biunify(ast.NewTerm(result), b, b1, b2, iter)
}

func (e *eval) biunifyComprehensionSet(x *ast.SetComprehension, b *ast.Term, b1, b2 *bindings, iter unifyIterator) error {
	result := ast.NewSet()
	child := evalPool.Get()

	e.closure(x.Body, child)
	defer evalPool.Put(child)

	err := child.Run(func(child *eval) error {
		result.Add(child.bindings.Plug(x.Term))
		return nil
	})
	if err != nil {
		return err
	}
	return e.biunify(ast.NewTerm(result), b, b1, b2, iter)
}

func (e *eval) biunifyComprehensionObject(x *ast.ObjectComprehension, b *ast.Term, b1, b2 *bindings, iter unifyIterator) error {
	child := evalPool.Get()
	defer evalPool.Put(child)

	e.closure(x.Body, child)

	result := ast.NewObject()

	err := child.Run(func(child *eval) error {
		key := child.bindings.Plug(x.Key)
		value := child.bindings.Plug(x.Value)
		exist := result.Get(key)
		if exist != nil && !exist.Equal(value) {
			return objectDocKeyConflictErr(x.Key.Location)
		}
		result.Insert(key, value)
		return nil
	})
	if err != nil {
		return err
	}
	return e.biunify(ast.NewTerm(result), b, b1, b2, iter)
}

func (e *eval) saveExpr(expr *ast.Expr, b *bindings, iter unifyIterator) error {
	e.updateFromQuery(expr)
	e.saveStack.Push(expr, b, b)
	e.traceSave(expr)
	err := iter()
	e.saveStack.Pop()
	return err
}

func (e *eval) saveExprMarkUnknowns(expr *ast.Expr, b *bindings, iter unifyIterator) error {
	e.updateFromQuery(expr)
	declArgsLen, err := e.getDeclArgsLen(expr)
	if err != nil {
		return err
	}
	var pops int
	if pairs := getSavePairsFromExpr(declArgsLen, expr, b, nil); len(pairs) > 0 {
		pops += len(pairs)
		for _, p := range pairs {
			e.saveSet.Push([]*ast.Term{p.term}, p.b)
		}
	}
	e.saveStack.Push(expr, b, b)
	e.traceSave(expr)
	err = iter()
	e.saveStack.Pop()
	for range pops {
		e.saveSet.Pop()
	}
	return err
}

func (e *eval) saveUnify(a, b *ast.Term, b1, b2 *bindings, iter unifyIterator) error {
	e.instr.startTimer(partialOpSaveUnify)
	expr := ast.Equality.Expr(a, b)
	e.updateFromQuery(expr)
	pops := 0
	if pairs := getSavePairsFromTerm(a, b1, nil); len(pairs) > 0 {
		pops += len(pairs)
		for _, p := range pairs {
			e.saveSet.Push([]*ast.Term{p.term}, p.b)
		}

	}
	if pairs := getSavePairsFromTerm(b, b2, nil); len(pairs) > 0 {
		pops += len(pairs)
		for _, p := range pairs {
			e.saveSet.Push([]*ast.Term{p.term}, p.b)
		}
	}
	e.saveStack.Push(expr, b1, b2)
	e.traceSave(expr)
	e.instr.stopTimer(partialOpSaveUnify)
	err := iter()

	e.saveStack.Pop()
	for range pops {
		e.saveSet.Pop()
	}

	return err
}

func (e *eval) saveCall(declArgsLen int, terms []*ast.Term, iter unifyIterator) error {
	expr := ast.NewExpr(terms)
	e.updateFromQuery(expr)

	// If call-site includes output value then partial eval must add vars in output
	// position to the save set.
	pops := 0
	if declArgsLen == len(terms)-2 {
		if pairs := getSavePairsFromTerm(terms[len(terms)-1], e.bindings, nil); len(pairs) > 0 {
			pops += len(pairs)
			for _, p := range pairs {
				e.saveSet.Push([]*ast.Term{p.term}, p.b)
			}
		}
	}
	e.saveStack.Push(expr, e.bindings, nil)
	e.traceSave(expr)
	err := iter()

	e.saveStack.Pop()
	for range pops {
		e.saveSet.Pop()
	}
	return err
}

func (e *eval) saveInlinedNegatedExprs(exprs []*ast.Expr, iter unifyIterator) error {

	with := make([]*ast.With, len(e.query[e.index].With))

	for i := range e.query[e.index].With {
		cpy := e.query[e.index].With[i].Copy()
		cpy.Value = e.bindings.PlugNamespaced(cpy.Value, e.caller.bindings)
		with[i] = cpy
	}

	for _, expr := range exprs {
		expr.With = e.updateSavedMocks(with)
		e.saveStack.Push(expr, nil, nil)
		e.traceSave(expr)
	}
	err := iter()
	for range exprs {
		e.saveStack.Pop()
	}
	return err
}

func (e *eval) getRules(ref ast.Ref, args []*ast.Term) (*ast.IndexResult, error) {
	e.instr.startTimer(evalOpRuleIndex)
	defer e.instr.stopTimer(evalOpRuleIndex)

	index := e.compiler.RuleIndex(ref)
	if index == nil {
		return nil, nil
	}

	resolver := resolverPool.Get().(*evalResolver)
	defer func() {
		resolver.e = nil
		resolver.args = nil
		resolverPool.Put(resolver)
	}()

	var result *ast.IndexResult
	var err error
	if e.indexing {
		resolver.e = e
		resolver.args = args
		result, err = index.Lookup(resolver)
	} else {
		resolver.e = e
		result, err = index.AllRules(resolver)
	}
	if err != nil {
		return nil, err
	}

	result.EarlyExit = result.EarlyExit && e.earlyExit

	if e.traceEnabled {
		var msg strings.Builder
		if len(result.Rules) == 1 {
			msg.WriteString("(matched 1 rule")
		} else {
			msg.Grow(len("(matched NNNN rules)"))
			msg.WriteString("(matched ")
			msg.WriteString(strconv.Itoa(len(result.Rules)))
			msg.WriteString(" rules")
		}
		if result.EarlyExit {
			msg.WriteString(", early exit")
		}
		msg.WriteRune(')')

		// Copy ref here as ref otherwise always escapes to the heap,
		// whether tracing is enabled or not.
		r := ref.Copy()
		e.traceIndex(e.query[e.index], msg.String(), &r)
	}

	return result, err
}

func (e *eval) Resolve(ref ast.Ref) (ast.Value, error) {
	return (&evalResolver{e: e}).Resolve(ref)
}

type evalResolver struct {
	e    *eval
	args []*ast.Term
}

var (
	resolverPool = sync.Pool{
		New: func() any {
			return &evalResolver{}
		},
	}
)

func (e *evalResolver) Resolve(ref ast.Ref) (ast.Value, error) {
	e.e.instr.startTimer(evalOpResolve)

	// NOTE(ae): nil check on saveSet to avoid ast.NewTerm allocation when not needed
	if e.e.inliningControl.Disabled(ref, true) || (e.e.saveSet != nil &&
		e.e.saveSet.Contains(ast.NewTerm(ref), nil)) {
		e.e.instr.stopTimer(evalOpResolve)
		return nil, ast.UnknownValueErr{}
	}

	// Lookup of function argument values works by using the `args` ref[0],
	// where the ast.Number in ref[1] references the function argument of
	// that number. The callsite-local arguments are passed in e.args,
	// indexed by argument index.
	if ref[0].Equal(ast.FunctionArgRootDocument) {
		v, ok := ref[1].Value.(ast.Number)
		if ok {
			i, ok := v.Int()
			if ok && i >= 0 && i < len(e.args) {
				e.e.instr.stopTimer(evalOpResolve)
				plugged := e.e.bindings.PlugNamespaced(e.args[i], e.e.caller.bindings)
				return plugged.Value, nil
			}
		}
		e.e.instr.stopTimer(evalOpResolve)
		return nil, ast.UnknownValueErr{}
	}

	if ref[0].Equal(ast.InputRootDocument) {
		if e.e.input != nil {
			v, err := e.e.input.Value.Find(ref[1:])
			if err != nil {
				v = nil
			}
			e.e.instr.stopTimer(evalOpResolve)
			return v, nil
		}
		e.e.instr.stopTimer(evalOpResolve)
		return nil, nil
	}

	if ref[0].Equal(ast.DefaultRootDocument) {

		var repValue ast.Value

		if e.e.data != nil {
			if v, err := e.e.data.Value.Find(ref[1:]); err == nil {
				repValue = v
			}
		}

		if e.e.targetStack.Prefixed(ref) {
			e.e.instr.stopTimer(evalOpResolve)
			return repValue, nil
		}

		var merged ast.Value
		var err error

		// Converting large JSON values into AST values can be fairly expensive. For
		// example, a 2MB JSON value can take upwards of 30 millisceonds to convert.
		// We cache the result of conversion here in case the same base document is
		// being read multiple times during evaluation.
		realValue := e.e.baseCache.Get(ref)
		if realValue != nil {
			e.e.instr.counterIncr(evalOpBaseCacheHit)
			if repValue == nil {
				e.e.instr.stopTimer(evalOpResolve)
				return realValue, nil
			}
			var ok bool
			merged, ok = merge(repValue, realValue)
			if !ok {
				err = mergeConflictErr(ref[0].Location)
			}
		} else { // baseCache miss
			e.e.instr.counterIncr(evalOpBaseCacheMiss)
			merged, err = e.e.resolveReadFromStorage(ref, repValue)
		}
		e.e.instr.stopTimer(evalOpResolve)
		return merged, err
	}
	e.e.instr.stopTimer(evalOpResolve)
	return nil, errors.New("illegal ref")
}

func (e *eval) resolveReadFromStorage(ref ast.Ref, a ast.Value) (ast.Value, error) {
	if refContainsNonScalar(ref) {
		return a, nil
	}

	v, err := e.external.Resolve(e, ref)
	if err != nil {
		return nil, err
	}
	if v == nil {
		path, err := storage.NewPathForRef(ref)
		if err != nil {
			if !storage.IsNotFound(err) {
				return nil, err
			}
			return a, nil
		}

		blob, err := e.store.Read(e.ctx, e.txn, path)
		if err != nil {
			if !storage.IsNotFound(err) {
				return nil, err
			}
			return a, nil
		}

		if len(path) == 0 {
			switch obj := blob.(type) {
			case map[string]any:
				if len(obj) > 0 {
					cpy := make(map[string]any, len(obj)-1)
					for k, v := range obj {
						if string(ast.SystemDocumentKey) != k {
							cpy[k] = v
						}
					}
					blob = cpy
				}
			case ast.Object:
				if obj.Len() > 0 {
					blob, _ = obj.Map(systemDocumentKeyRemoveMapper)
				}
			}
		}

		switch blob := blob.(type) {
		case ast.Value:
			v = blob
		default:
			if blob, ok := blob.(map[string]any); ok && !e.strictObjects {
				v = ast.LazyObject(blob)
				break
			}
			v, err = ast.InterfaceToValue(blob)
			if err != nil {
				return nil, err
			}
		}
	}

	e.baseCache.Put(ref, v)
	if a == nil {
		return v, nil
	}

	merged, ok := merge(a, v)
	if !ok {
		return nil, mergeConflictErr(ref[0].Location)
	}

	return merged, nil
}

func systemDocumentKeyRemoveMapper(k, v *ast.Term) (*ast.Term, *ast.Term, error) {
	if ast.SystemDocumentKey.Equal(k.Value) {
		return nil, nil, nil
	}
	return k, v, nil
}

func (e *eval) generateVar(suffix string) *ast.Term {
	buf := make([]byte, 0, len(e.genvarprefix)+len(suffix)+1)

	buf = append(buf, e.genvarprefix...)
	buf = append(buf, '_')
	buf = append(buf, suffix...)

	return ast.VarTerm(util.ByteSliceToString(buf))
}

func (e *eval) rewrittenVar(v ast.Var) (ast.Var, bool) {
	if e.compiler != nil {
		if rw, ok := e.compiler.RewrittenVars[v]; ok {
			return rw, true
		}
	}
	if e.queryCompiler != nil {
		if rw, ok := e.queryCompiler.RewrittenVars()[v]; ok {
			return rw, true
		}
	}
	return v, false
}

func (e *eval) getDeclArgsLen(x *ast.Expr) (int, error) {

	if !x.IsCall() {
		return -1, nil
	}

	operator := x.Operator()
	bi, _, ok := e.builtinFunc(operator.String())

	if ok {
		return bi.Decl.Arity(), nil
	}

	ir, err := e.getRules(operator, nil)
	defer ast.IndexResultPool.Put(ir)
	if err != nil {
		return -1, err
	} else if ir == nil || ir.Empty() {
		return -1, nil
	}

	return len(ir.Rules[0].Head.Args), nil
}

// updateFromQuery enriches the passed expression with Location and With
// fields of the currently looked-at query item (`e.query[e.index]`).
// With values are namespaced to ensure that replacement functions of
// mocked built-ins are properly referenced in the support module.
func (e *eval) updateFromQuery(expr *ast.Expr) {
	expr.With = e.updateSavedMocks(e.query[e.index].With)
	expr.Location = e.query[e.index].Location
}

type evalBuiltin struct {
	e     *eval
	bi    *ast.Builtin
	bctx  *BuiltinContext
	f     BuiltinFunc
	terms []*ast.Term
}

// Is this builtin non-deterministic, and did the caller provide an NDBCache?
func (e *evalBuiltin) canUseNDBCache(bi *ast.Builtin) bool {
	return bi.Nondeterministic && e.bctx != nil && e.bctx.NDBuiltinCache != nil
}

func (e *evalBuiltin) eval(iter unifyIterator) error {

	operands := make([]*ast.Term, len(e.terms))

	for i := range e.terms {
		operands[i] = e.e.bindings.Plug(e.terms[i])
	}

	numDeclArgs := e.bi.Decl.Arity()

	e.e.instr.startTimer(evalOpBuiltinCall)

	// NOTE(philipc): We sometimes have to drop the very last term off
	// the args list for cases where a builtin's result is used/assigned,
	// because the last term will be a generated term, not an actual
	// argument to the builtin.
	endIndex := len(operands)
	if len(operands) > numDeclArgs {
		endIndex--
	}

	// We skip evaluation of the builtin entirely if the NDBCache is
	// present, and we have a non-deterministic builtin already cached.
	if e.canUseNDBCache(e.bi) {
		e.e.instr.stopTimer(evalOpBuiltinCall)

		// Unify against the NDBCache result if present.
		if v, ok := e.bctx.NDBuiltinCache.Get(e.bi.Name, ast.NewArray(operands[:endIndex]...)); ok {
			switch {
			case e.bi.Decl.Result() == nil:
				return iter()
			case len(operands) == numDeclArgs:
				if ast.Boolean(false).Equal(v) {
					return nil // nothing to do
				}
				return iter()
			default:
				return e.e.unify(e.terms[endIndex], ast.NewTerm(v), iter)
			}
		}

		// Otherwise, we'll need to go through the normal unify flow.
		e.e.instr.startTimer(evalOpBuiltinCall)
	}

	var bctx BuiltinContext
	if e.bctx == nil {
		bctx = BuiltinContext{
			// Location potentially needed for error reporting.
			Location: e.e.query[e.e.index].Location,
		}
	} else {
		bctx = *e.bctx
	}

	// Normal unification flow for builtins:
	err := e.f(bctx, operands, func(output *ast.Term) error {

		e.e.instr.stopTimer(evalOpBuiltinCall)

		var err error

		switch {
		case e.bi.Decl.Result() == nil:
			err = iter()
		case len(operands) == numDeclArgs:
			if !ast.Boolean(false).Equal(output.Value) {
				err = iter()
			} // else: nothing to do, don't iter()
		default:
			err = e.e.unify(e.terms[endIndex], output, iter)
		}

		// If the NDBCache is present, we can assume this builtin
		// call was not cached earlier.
		if e.canUseNDBCache(e.bi) {
			// Populate the NDBCache from the output term.
			e.bctx.NDBuiltinCache.Put(e.bi.Name, ast.NewArray(operands[:endIndex]...), output.Value)
		}

		if err != nil {
			// NOTE(sr): We wrap the errors here into Halt{} because we don't want to
			// record them into builtinErrors below. The errors set here are coming from
			// the call to iter(), not from the builtin implementation.
			err = Halt{Err: err}
		}

		e.e.instr.startTimer(evalOpBuiltinCall)
		return err
	})

	if err != nil {
		if t, ok := err.(Halt); ok {
			err = t.Err
		} else {
			e.e.builtinErrors.errs = append(e.e.builtinErrors.errs, err)
			err = nil
		}
	}

	e.e.instr.stopTimer(evalOpBuiltinCall)
	return err
}

type evalFunc struct {
	e     *eval
	ir    *ast.IndexResult
	terms []*ast.Term
}

func (e evalFunc) eval(iter unifyIterator) error {

	if e.ir.Empty() {
		return nil
	}

	var argCount int
	if len(e.ir.Rules) > 0 {
		argCount = len(e.ir.Rules[0].Head.Args)
	} else if e.ir.Default != nil {
		argCount = len(e.ir.Default.Head.Args)
	}

	if len(e.ir.Else) > 0 && e.e.unknown(e.e.query[e.e.index], e.e.bindings) {
		// Partial evaluation of ordered rules is not supported currently. Save the
		// expression and continue. This could be revisited in the future.
		return e.e.saveCall(argCount, e.terms, iter)
	}

	if e.e.partial() {
		var mustGenerateSupport bool

		if defRule := e.ir.Default; defRule != nil {
			// The presence of a default func might force us to generate support
			if len(defRule.Head.Args) == len(e.terms)-1 {
				// The function is called without collecting the result in an output term,
				// therefore any successful evaluation of the function is of interest, including the default value ...
				if ret := defRule.Head.Value; ret == nil || !ret.Equal(ast.InternedTerm(false)) {
					// ... unless the default value is false,
					mustGenerateSupport = true
				}
			} else {
				// The function is called with an output term, therefore any successful evaluation of the function is of interest.
				// NOTE: Because of how the compiler rewrites function calls, we can't know if the result value is compared
				// to a constant value, so we can't be as clever as we are for rules.
				mustGenerateSupport = true
			}
		}

		ref := e.terms[0].Value.(ast.Ref)

		if mustGenerateSupport || e.e.inliningControl.shallow || e.e.inliningControl.Disabled(ref, false) {
			// check if the function definitions, or any of the arguments
			// contain something unknown
			unknown := e.e.unknownRef(ref, e.e.bindings)
			for i := 1; !unknown && i <= argCount; i++ {
				unknown = e.e.unknown(e.terms[i], e.e.bindings)
			}
			if unknown {
				return e.partialEvalSupport(argCount, iter)
			}
		}
	}

	return e.evalValue(iter, argCount, e.ir.EarlyExit)
}

func (e evalFunc) evalValue(iter unifyIterator, argCount int, findOne bool) error {
	var cacheKey ast.Ref
	if !e.e.partial() {
		var hit bool
		var err error
		cacheKey, hit, err = e.evalCache(argCount, iter)
		if err != nil {
			return err
		} else if hit {
			return nil
		}
	}

	// NOTE(anders): While it makes the code a bit more complex, reusing the
	// args slice across each function increment saves a lot of resources
	// compared to creating a new one inside each call to evalOneRule... so
	// think twice before simplifying this :)
	args := make([]*ast.Term, len(e.terms)-1)

	var prev *ast.Term

	return withSuppressEarlyExit(func() error {
		var outerEe *deferredEarlyExitError
		for _, rule := range e.ir.Rules {
			copy(args, rule.Head.Args)
			if len(args) == len(rule.Head.Args)+1 {
				args[len(args)-1] = rule.Head.Value
			}

			next, err := e.evalOneRule(iter, rule, args, cacheKey, prev, findOne)
			if err != nil {
				if oee, ok := err.(*deferredEarlyExitError); ok {
					if outerEe == nil {
						outerEe = oee
					}
				} else {
					return err
				}
			}
			if next == nil {
				for _, erule := range e.ir.Else[rule] {
					copy(args, erule.Head.Args)
					if len(args) == len(erule.Head.Args)+1 {
						args[len(args)-1] = erule.Head.Value
					}

					next, err = e.evalOneRule(iter, erule, args, cacheKey, prev, findOne)
					if err != nil {
						if oee, ok := err.(*deferredEarlyExitError); ok {
							if outerEe == nil {
								outerEe = oee
							}
						} else {
							return err
						}
					}
					if next != nil {
						break
					}
				}
			}
			if next != nil {
				prev = next
			}
		}

		if e.ir.Default != nil && prev == nil {
			copy(args, e.ir.Default.Head.Args)
			if len(args) == len(e.ir.Default.Head.Args)+1 {
				args[len(args)-1] = e.ir.Default.Head.Value
			}

			_, err := e.evalOneRule(iter, e.ir.Default, args, cacheKey, prev, findOne)

			return err
		}

		if outerEe != nil {
			return outerEe
		}

		return nil
	})
}

func (e evalFunc) evalCache(argCount int, iter unifyIterator) (ast.Ref, bool, error) {
	plen := len(e.terms)
	if plen == argCount+2 { // func name + output = 2
		plen -= 1
	}

	cacheKey := make([]*ast.Term, plen)
	for i := range plen {
		if e.terms[i].IsGround() {
			// Avoid expensive copying of ref if it is ground.
			cacheKey[i] = e.terms[i]
		} else {
			cacheKey[i] = e.e.bindings.Plug(e.terms[i])
		}
	}

	cached, _ := e.e.virtualCache.Get(cacheKey)
	if cached != nil {
		e.e.instr.counterIncr(evalOpVirtualCacheHit)
		if argCount == len(e.terms)-1 { // f(x)
			if ast.Boolean(false).Equal(cached.Value) {
				return nil, true, nil
			}
			return nil, true, iter()
		}
		// f(x, y), y captured output value
		return nil, true, e.e.unify(e.terms[len(e.terms)-1] /* y */, cached, iter)
	}
	e.e.instr.counterIncr(evalOpVirtualCacheMiss)
	return cacheKey, false, nil
}

func (e evalFunc) evalOneRule(iter unifyIterator, rule *ast.Rule, args []*ast.Term, cacheKey ast.Ref, prev *ast.Term, findOne bool) (*ast.Term, error) {
	child := evalPool.Get()
	defer evalPool.Put(child)

	e.e.child(rule.Body, child)
	child.findOne = findOne

	var result *ast.Term

	child.traceEnter(rule)

	err := child.biunifyTerms(e.terms[1:], args, e.e.bindings, child.bindings, func() error {
		return child.eval(func(child *eval) error {
			child.traceExit(rule)

			// Partial evaluation must save an expression that tests the output value if the output value
			// was not captured to handle the case where the output value may be `false`.
			if len(rule.Head.Args) == len(e.terms)-1 && e.e.saveSet.Contains(rule.Head.Value, child.bindings) {
				err := e.e.saveExpr(ast.NewExpr(rule.Head.Value), child.bindings, iter)
				child.traceRedo(rule)
				return err
			}

			result = child.bindings.Plug(rule.Head.Value)
			if cacheKey != nil {
				e.e.virtualCache.Put(cacheKey, result) // the redos confirm this, or the evaluation is aborted
			}

			if len(rule.Head.Args) == len(e.terms)-1 && ast.Boolean(false).Equal(result.Value) {
				if prev != nil && !prev.Equal(result) {
					return functionConflictErr(rule.Location)
				}
				prev = result
				return nil
			}

			// Partial evaluation should explore all rules and may not produce
			// a ground result so we do not perform conflict detection or
			// deduplication. See "ignore conflicts: functions" test case for
			// an example.
			if !e.e.partial() && prev != nil {
				if !prev.Equal(result) {
					return functionConflictErr(rule.Location)
				}
				child.traceRedo(rule)
				return nil
			}

			prev = result

			if err := iter(); err != nil {
				return err
			}

			child.traceRedo(rule)
			return nil
		})
	})

	return result, err
}

func (e evalFunc) partialEvalSupport(declArgsLen int, iter unifyIterator) error {
	path := e.e.namespaceRef(e.terms[0].Value.(ast.Ref))

	if !e.e.saveSupport.Exists(path) {
		for _, rule := range e.ir.Rules {
			err := e.partialEvalSupportRule(rule, path)
			if err != nil {
				return err
			}
		}

		if e.ir.Default != nil {
			err := e.partialEvalSupportRule(e.ir.Default, path)
			if err != nil {
				return err
			}
		}
	}

	if !e.e.saveSupport.Exists(path) { // we haven't saved anything, nothing to call
		return nil
	}

	term := ast.NewTerm(path)

	return e.e.saveCall(declArgsLen, append([]*ast.Term{term}, e.terms[1:]...), iter)
}

func (e evalFunc) partialEvalSupportRule(rule *ast.Rule, path ast.Ref) error {
	child := evalPool.Get()
	defer evalPool.Put(child)

	e.e.child(rule.Body, child)
	child.traceEnter(rule)

	e.e.saveStack.PushQuery(nil)

	// treat the function arguments as unknown during rule body evaluation
	var args []*ast.Term
	ast.WalkVars(rule.Head.Args, func(v ast.Var) bool {
		args = append(args, ast.VarTerm(string(v)))
		return false
	})
	e.e.saveSet.Push(args, child.bindings)

	err := child.eval(func(child *eval) error {
		child.traceExit(rule)

		current := e.e.saveStack.PopQuery()
		plugged := current.Plug(e.e.caller.bindings)

		// Skip this rule body if it fails to type-check.
		// Type-checking failure means the rule body will never succeed.
		if e.e.compiler.PassesTypeCheck(plugged) {
			head := &ast.Head{
				Name:      rule.Head.Name,
				Reference: rule.Head.Reference,
				Value:     child.bindings.PlugNamespaced(rule.Head.Value, e.e.caller.bindings),
				Args:      make([]*ast.Term, len(rule.Head.Args)),
			}
			for i, a := range rule.Head.Args {
				head.Args[i] = child.bindings.PlugNamespaced(a, e.e.caller.bindings)
			}

			e.e.saveSupport.Insert(path, &ast.Rule{
				Head:    head,
				Body:    plugged,
				Default: rule.Default,
			})
		}
		child.traceRedo(rule)
		e.e.saveStack.PushQuery(current)
		return nil
	})

	e.e.saveSet.Pop()
	e.e.saveStack.PopQuery()
	return err
}

type deferredEarlyExitContainer struct {
	deferred *deferredEarlyExitError
}

func (dc *deferredEarlyExitContainer) handleErr(err error) error {
	if err == nil {
		return nil
	}

	if dc.deferred == nil && errors.As(err, &dc.deferred) && dc.deferred != nil {
		return nil
	}

	return err
}

// copyError returns a copy of the deferred early exit error if one is present.
// This exists only to allow the container to be reused.
func (dc *deferredEarlyExitContainer) copyError() *deferredEarlyExitError {
	if dc.deferred == nil {
		return nil
	}

	cpy := *dc.deferred
	return &cpy
}

var deecPool = sync.Pool{
	New: func() any {
		return &deferredEarlyExitContainer{}
	},
}

type evalTree struct {
	e         *eval
	bindings  *bindings
	rterm     *ast.Term
	rbindings *bindings
	node      *ast.TreeNode
	ref       ast.Ref
	plugged   ast.Ref
	pos       int
}

func (e evalTree) eval(iter unifyIterator) error {

	if len(e.ref) == e.pos {
		return e.finish(iter)
	}

	plugged := e.bindings.Plug(e.ref[e.pos])

	if plugged.IsGround() {
		return e.next(iter, plugged)
	}

	return e.enumerate(iter)
}

func (e evalTree) finish(iter unifyIterator) error {

	// In some cases, it may not be possible to PE the ref. If the path refers
	// to virtual docs that PE does not support or base documents where inlining
	// has been disabled, then we have to save.
	if e.e.partial() && e.e.unknownRef(e.plugged, e.e.bindings) {
		return e.e.saveUnify(ast.NewTerm(e.plugged), e.rterm, e.bindings, e.rbindings, iter)
	}

	v, err := e.extent()
	if err != nil || v == nil {
		return err
	}

	return e.e.biunify(e.rterm, v, e.rbindings, e.bindings, iter)
}

func (e evalTree) next(iter unifyIterator, plugged *ast.Term) error {

	var node *ast.TreeNode

	cpy := e
	cpy.plugged[e.pos] = plugged
	cpy.pos++

	if !e.e.targetStack.Prefixed(cpy.plugged[:cpy.pos]) {
		if e.node != nil {
			node = e.node.Child(plugged.Value)
			if node != nil && len(node.Values) > 0 {
				r := evalVirtual{
					e:         e.e,
					ref:       e.ref,
					plugged:   e.plugged,
					pos:       e.pos,
					bindings:  e.bindings,
					rterm:     e.rterm,
					rbindings: e.rbindings,
				}
				r.plugged[e.pos] = plugged
				return r.eval(iter)
			}
		}
	}

	cpy.node = node
	return cpy.eval(iter)
}

func (e evalTree) enumerate(iter unifyIterator) error {

	if e.e.inliningControl.Disabled(e.plugged[:e.pos], true) {
		return e.e.saveUnify(ast.NewTerm(e.plugged), e.rterm, e.bindings, e.rbindings, iter)
	}

	doc, err := e.e.Resolve(e.plugged[:e.pos])
	if err != nil {
		return err
	}

	dc := deecPool.Get().(*deferredEarlyExitContainer)
	dc.deferred = nil
	defer deecPool.Put(dc)

	if doc != nil {
		switch doc := doc.(type) {
		case *ast.Array:
			for i := range doc.Len() {
				k := ast.InternedTerm(i)
				err := e.e.biunify(k, e.ref[e.pos], e.bindings, e.bindings, func() error {
					return e.next(iter, k)
				})

				if err := dc.handleErr(err); err != nil {
					return err
				}
			}
		case ast.Object:
			ki := doc.KeysIterator()
			for k, more := ki.Next(); more; k, more = ki.Next() {
				err := e.e.biunify(k, e.ref[e.pos], e.bindings, e.bindings, func() error {
					return e.next(iter, k)
				})
				if err := dc.handleErr(err); err != nil {
					return err
				}
			}
		case ast.Set:
			if err := doc.Iter(func(elem *ast.Term) error {
				err := e.e.biunify(elem, e.ref[e.pos], e.bindings, e.bindings, func() error {
					return e.next(iter, elem)
				})
				return dc.handleErr(err)
			}); err != nil {
				return err
			}
		}
	}

	if dc.deferred != nil {
		return dc.copyError()
	}

	if e.node == nil {
		return nil
	}

	for _, k := range e.node.Sorted {
		key := ast.NewTerm(k)
		if err := e.e.biunify(key, e.ref[e.pos], e.bindings, e.bindings, func() error {
			return e.next(iter, key)
		}); err != nil {
			return err
		}
	}

	return nil
}

func (e evalTree) extent() (*ast.Term, error) {
	base, err := e.e.Resolve(e.plugged)
	if err != nil {
		return nil, err
	}

	virtual, err := e.leaves(e.plugged, e.node)
	if err != nil {
		return nil, err
	}

	if virtual == nil {
		if base == nil {
			return nil, nil
		}
		return ast.NewTerm(base), nil
	}

	if base != nil {
		merged, ok := merge(base, virtual)
		if !ok {
			return nil, mergeConflictErr(e.plugged[0].Location)
		}
		return ast.NewTerm(merged), nil
	}

	return ast.NewTerm(virtual), nil
}

// leaves builds a tree from evaluating the full rule tree extent, by recursing into all
// branches, and building up objects as it goes.
func (e evalTree) leaves(plugged ast.Ref, node *ast.TreeNode) (ast.Object, error) {

	if e.node == nil {
		return nil, nil
	}

	result := ast.NewObject()

	for _, k := range node.Sorted {

		child := node.Children[k]

		if child.Hide {
			continue
		}

		plugged = append(plugged, ast.NewTerm(child.Key))

		var save ast.Value
		var err error

		if len(child.Values) > 0 {
			rterm := e.e.generateVar("leaf")
			err = e.e.unify(ast.NewTerm(plugged), rterm, func() error {
				save = e.e.bindings.Plug(rterm).Value
				return nil
			})
		} else {
			save, err = e.leaves(plugged, child)
		}

		if err != nil {
			return nil, err
		}

		if save != nil {
			v := ast.NewObject([2]*ast.Term{plugged[len(plugged)-1], ast.NewTerm(save)})
			result, _ = result.Merge(v)
		}

		plugged = plugged[:len(plugged)-1]
	}

	return result, nil
}

type evalVirtual struct {
	e         *eval
	bindings  *bindings
	rterm     *ast.Term
	rbindings *bindings
	ref       ast.Ref
	plugged   ast.Ref
	pos       int
}

func (e evalVirtual) eval(iter unifyIterator) error {

	ir, err := e.e.getRules(e.plugged[:e.pos+1], nil)
	defer ast.IndexResultPool.Put(ir)
	if err != nil {
		return err
	}

	// Partial evaluation of ordered rules is not supported currently. Save the
	// expression and continue. This could be revisited in the future.
	if len(ir.Else) > 0 && e.e.unknownRef(e.ref, e.bindings) {
		return e.e.saveUnify(ast.NewTerm(e.ref), e.rterm, e.bindings, e.rbindings, iter)
	}

	switch ir.Kind {
	case ast.MultiValue:
		var empty *ast.Term
		if ir.OnlyGroundRefs {
			// rule ref contains no vars, so we're building a set
			empty = ast.SetTerm()
		} else {
			// rule ref contains vars, so we're building an object containing a set leaf
			empty = ast.ObjectTerm()
		}
		eval := evalVirtualPartial{
			e:         e.e,
			ref:       e.ref,
			plugged:   e.plugged,
			pos:       e.pos,
			ir:        ir,
			bindings:  e.bindings,
			rterm:     e.rterm,
			rbindings: e.rbindings,
			empty:     empty,
		}
		return eval.eval(iter)
	case ast.SingleValue:
		if ir.OnlyGroundRefs {
			eval := evalVirtualComplete{
				e:         e.e,
				ref:       e.ref,
				plugged:   e.plugged,
				pos:       e.pos,
				ir:        ir,
				bindings:  e.bindings,
				rterm:     e.rterm,
				rbindings: e.rbindings,
			}
			return eval.eval(iter)
		}
		eval := evalVirtualPartial{
			e:         e.e,
			ref:       e.ref,
			plugged:   e.plugged,
			pos:       e.pos,
			ir:        ir,
			bindings:  e.bindings,
			rterm:     e.rterm,
			rbindings: e.rbindings,
			empty:     ast.ObjectTerm(),
		}
		return eval.eval(iter)
	default:
		panic("unreachable")
	}
}

type evalVirtualPartial struct {
	e         *eval
	ir        *ast.IndexResult
	bindings  *bindings
	rterm     *ast.Term
	rbindings *bindings
	empty     *ast.Term
	ref       ast.Ref
	plugged   ast.Ref
	pos       int
}

type evalVirtualPartialCacheHint struct {
	key  ast.Ref
	hit  bool
	full bool
}

func (h *evalVirtualPartialCacheHint) keyWithoutScope() ast.Ref {
	if h.key != nil {
		if _, ok := h.key[len(h.key)-1].Value.(vcKeyScope); ok {
			return h.key[:len(h.key)-1]
		}
	}
	return h.key
}

func (e evalVirtualPartial) eval(iter unifyIterator) error {

	unknown := e.e.unknown(e.ref[:e.pos+1], e.bindings)

	if len(e.ref) == e.pos+1 {
		if unknown {
			return e.partialEvalSupport(iter)
		}
		return e.evalAllRules(iter, e.ir.Rules)
	}

	if (unknown && e.e.inliningControl.shallow) || e.e.inliningControl.Disabled(e.ref[:e.pos+1], false) {
		return e.partialEvalSupport(iter)
	}

	return e.evalEachRule(iter, unknown)
}

// returns the maximum length a ref can be without being longer than the longest rule ref in rules.
func maxRefLength(rules []*ast.Rule, ceil int) int {
	var l int
	for _, r := range rules {
		rl := len(r.Ref())
		if r.Head.RuleKind() == ast.MultiValue {
			rl++
		}
		if rl >= ceil {
			return ceil
		} else if rl > l {
			l = rl
		}
	}
	return l
}

func (e evalVirtualPartial) evalEachRule(iter unifyIterator, unknown bool) error {

	if e.ir.Empty() {
		return nil
	}

	if e.e.partial() {
		m := maxRefLength(e.ir.Rules, len(e.ref))
		if e.e.unknown(e.ref[e.pos+1:m], e.bindings) {
			for _, rule := range e.ir.Rules {
				if err := e.evalOneRulePostUnify(iter, rule); err != nil {
					return err
				}
			}
			return nil
		}
	}

	hint, err := e.evalCache(iter)
	if err != nil {
		return err
	} else if hint.hit {
		return nil
	}

	if hint.full {
		result, err := e.evalAllRulesNoCache(e.ir.Rules)
		if err != nil {
			return err
		}
		e.e.virtualCache.Put(hint.key, result)
		return e.evalTerm(iter, e.pos+1, result, e.bindings)
	}

	result := e.empty
	var visitedRefs []ast.Ref

	for _, rule := range e.ir.Rules {
		result, err = e.evalOneRulePreUnify(iter, rule, result, unknown, &visitedRefs)
		if err != nil {
			return err
		}
	}

	if hint.key != nil {
		if v, err := result.Value.Find(hint.keyWithoutScope()[e.pos+1:]); err == nil && v != nil {
			e.e.virtualCache.Put(hint.key, ast.NewTerm(v))
		}
	}

	if !unknown {
		return e.evalTerm(iter, e.pos+1, result, e.bindings)
	}

	return nil
}

func (e evalVirtualPartial) evalAllRules(iter unifyIterator, rules []*ast.Rule) error {

	cacheKey := e.plugged[:e.pos+1]
	result, _ := e.e.virtualCache.Get(cacheKey)
	if result != nil {
		e.e.instr.counterIncr(evalOpVirtualCacheHit)
		return e.e.biunify(result, e.rterm, e.bindings, e.rbindings, iter)
	}

	e.e.instr.counterIncr(evalOpVirtualCacheMiss)

	result, err := e.evalAllRulesNoCache(rules)
	if err != nil {
		return err
	}

	if cacheKey != nil {
		e.e.virtualCache.Put(cacheKey, result)
	}

	return e.e.biunify(result, e.rterm, e.bindings, e.rbindings, iter)
}

func (e evalVirtualPartial) evalAllRulesNoCache(rules []*ast.Rule) (*ast.Term, error) {
	result := e.empty

	var visitedRefs []ast.Ref

	child := evalPool.Get()
	defer evalPool.Put(child)

	for _, rule := range rules {
		e.e.child(rule.Body, child)
		child.traceEnter(rule)
		err := child.eval(func(*eval) error {
			child.traceExit(rule)
			var err error
			result, _, err = e.reduce(rule, child.bindings, result, &visitedRefs)
			if err != nil {
				return err
			}

			child.traceRedo(rule)
			return nil
		})

		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

func wrapInObjects(leaf *ast.Term, ref ast.Ref) *ast.Term {
	// We build the nested objects leaf-to-root to preserve ground:ness
	if len(ref) == 0 {
		return leaf
	}
	key := ref[0]
	val := wrapInObjects(leaf, ref[1:])
	return ast.ObjectTerm(ast.Item(key, val))
}

func (e evalVirtualPartial) evalOneRulePreUnify(iter unifyIterator, rule *ast.Rule, result *ast.Term, unknown bool, visitedRefs *[]ast.Ref) (*ast.Term, error) {
	child := evalPool.Get()
	defer evalPool.Put(child)

	e.e.child(rule.Body, child)

	child.traceEnter(rule)
	var defined bool

	headKey := rule.Head.Key
	if headKey == nil {
		headKey = rule.Head.Reference[len(rule.Head.Reference)-1]
	}

	// Walk the dynamic portion of rule ref and key to unify vars
	err := child.biunifyRuleHead(e.pos+1, e.ref, rule, e.bindings, child.bindings, func(_ int) error {
		defined = true
		return child.eval(func(child *eval) error {

			child.traceExit(rule)

			term := rule.Head.Value
			if term == nil {
				term = headKey
			}

			if unknown {
				term, termbindings := child.bindings.apply(term)

				if rule.Head.RuleKind() == ast.MultiValue {
					term = ast.SetTerm(term)
				}

				objRef := rule.Ref()[e.pos+1:]
				term = wrapInObjects(term, objRef)

				err := e.evalTerm(iter, e.pos+1, term, termbindings)
				if err != nil {
					return err
				}
			} else {
				var dup bool
				var err error
				result, dup, err = e.reduce(rule, child.bindings, result, visitedRefs)
				if err != nil {
					return err
				} else if !unknown && dup {
					child.traceDuplicate(rule)
					return nil
				}
			}

			child.traceRedo(rule)

			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	if !defined {
		child.traceFail(rule)
	}

	return result, nil
}

func (e *eval) biunifyRuleHead(pos int, ref ast.Ref, rule *ast.Rule, refBindings, ruleBindings *bindings, iter unifyRefIterator) error {
	return e.biunifyDynamicRef(pos, ref, rule.Ref(), refBindings, ruleBindings, func(pos int) error {
		// FIXME: Is there a simpler, more robust way of figuring out that we should biunify the rule key?
		if rule.Head.RuleKind() == ast.MultiValue && pos < len(ref) && len(rule.Ref()) <= len(ref) {
			headKey := rule.Head.Key
			if headKey == nil {
				headKey = rule.Head.Reference[len(rule.Head.Reference)-1]
			}
			return e.biunify(ref[pos], headKey, refBindings, ruleBindings, func() error {
				return iter(pos + 1)
			})
		}
		return iter(pos)
	})
}

func (e *eval) biunifyDynamicRef(pos int, a, b ast.Ref, b1, b2 *bindings, iter unifyRefIterator) error {
	if pos >= len(a) || pos >= len(b) {
		return iter(pos)
	}

	return e.biunify(a[pos], b[pos], b1, b2, func() error {
		return e.biunifyDynamicRef(pos+1, a, b, b1, b2, iter)
	})
}

func (e evalVirtualPartial) evalOneRulePostUnify(iter unifyIterator, rule *ast.Rule) error {
	child := evalPool.Get()
	defer evalPool.Put(child)

	e.e.child(rule.Body, child)

	child.traceEnter(rule)
	var defined bool

	err := child.eval(func(child *eval) error {
		defined = true
		return e.e.biunifyRuleHead(e.pos+1, e.ref, rule, e.bindings, child.bindings, func(_ int) error {
			return e.evalOneRuleContinue(iter, rule, child)
		})
	})

	if err != nil {
		return err
	}

	if !defined {
		child.traceFail(rule)
	}

	return nil
}

func (e evalVirtualPartial) evalOneRuleContinue(iter unifyIterator, rule *ast.Rule, child *eval) error {

	child.traceExit(rule)

	term := rule.Head.Value
	if term == nil {
		term = rule.Head.Key
	}

	term, termbindings := child.bindings.apply(term)

	if rule.Head.RuleKind() == ast.MultiValue {
		term = ast.SetTerm(term)
	}

	objRef := rule.Ref()[e.pos+1:]
	term = wrapInObjects(term, objRef)

	err := e.evalTerm(iter, e.pos+1, term, termbindings)
	if err != nil {
		return err
	}

	child.traceRedo(rule)
	return nil
}

func (e evalVirtualPartial) partialEvalSupport(iter unifyIterator) error {

	path := e.e.namespaceRef(e.plugged[:e.pos+1])
	term := ast.NewTerm(e.e.namespaceRef(e.ref))

	var defined bool

	if e.e.saveSupport.Exists(path) {
		defined = true
	} else {
		for i := range e.ir.Rules {
			ok, err := e.partialEvalSupportRule(e.ir.Rules[i], path)
			if err != nil {
				return err
			}
			if ok {
				defined = true
			}
		}
	}

	if !defined {
		if len(e.ref) != e.pos+1 {
			return nil
		}

		// the entire partial set/obj was queried, e.g. data.a.q (not data.a.q[x])
		term = e.empty
	}

	return e.e.saveUnify(term, e.rterm, e.bindings, e.rbindings, iter)
}

func (e evalVirtualPartial) partialEvalSupportRule(rule *ast.Rule, _ ast.Ref) (bool, error) {
	child := evalPool.Get()
	defer evalPool.Put(child)

	e.e.child(rule.Body, child)
	child.traceEnter(rule)

	e.e.saveStack.PushQuery(nil)
	var defined bool

	err := child.eval(func(child *eval) error {
		child.traceExit(rule)
		defined = true

		current := e.e.saveStack.PopQuery()
		plugged := current.Plug(e.e.caller.bindings)
		// Skip this rule body if it fails to type-check.
		// Type-checking failure means the rule body will never succeed.
		if e.e.compiler.PassesTypeCheck(plugged) {
			var value *ast.Term

			if rule.Head.Value != nil {
				value = child.bindings.PlugNamespaced(rule.Head.Value, e.e.caller.bindings)
			}

			ref := e.e.namespaceRef(rule.Ref())
			for i := 1; i < len(ref); i++ {
				ref[i] = child.bindings.plugNamespaced(ref[i], e.e.caller.bindings)
			}
			pkg, ruleRef := splitPackageAndRule(ref)

			head := ast.RefHead(ruleRef, value)

			// key is also part of ref in single-value rules, and can be dropped
			if rule.Head.Key != nil && rule.Head.RuleKind() == ast.MultiValue {
				head.Key = child.bindings.PlugNamespaced(rule.Head.Key, e.e.caller.bindings)
			}

			if rule.Head.RuleKind() == ast.SingleValue && len(ruleRef) == 2 {
				head.Key = ruleRef[len(ruleRef)-1]
			}

			if head.Name.Equal(ast.Var("")) && (len(ruleRef) == 1 || (len(ruleRef) == 2 && rule.Head.RuleKind() == ast.SingleValue)) {
				head.Name = ruleRef[0].Value.(ast.Var)
			}

			if !e.e.inliningControl.shallow {
				cp := copypropagation.New(head.Vars()).
					WithEnsureNonEmptyBody(true).
					WithCompiler(e.e.compiler)
				plugged = applyCopyPropagation(cp, e.e.instr, plugged)
			}

			e.e.saveSupport.InsertByPkg(pkg, &ast.Rule{
				Head:    head,
				Body:    plugged,
				Default: rule.Default,
			})
		}
		child.traceRedo(rule)
		e.e.saveStack.PushQuery(current)
		return nil
	})
	e.e.saveStack.PopQuery()
	return defined, err
}

func (e evalVirtualPartial) evalTerm(iter unifyIterator, pos int, term *ast.Term, termbindings *bindings) error {
	eval := evalTerm{
		e:            e.e,
		ref:          e.ref,
		pos:          pos,
		bindings:     e.bindings,
		term:         term,
		termbindings: termbindings,
		rterm:        e.rterm,
		rbindings:    e.rbindings,
	}
	return eval.eval(iter)
}

func (e evalVirtualPartial) evalCache(iter unifyIterator) (evalVirtualPartialCacheHint, error) {

	var hint evalVirtualPartialCacheHint

	if e.e.unknown(e.ref[:e.pos+1], e.bindings) {
		// FIXME: Return empty hint if unknowns in any e.ref elem overlapping with applicable rule refs?
		return hint, nil
	}

	if cached, _ := e.e.virtualCache.Get(e.plugged[:e.pos+1]); cached != nil { // have full extent cached
		e.e.instr.counterIncr(evalOpVirtualCacheHit)
		hint.hit = true
		return hint, e.evalTerm(iter, e.pos+1, cached, e.bindings)
	}

	plugged := e.bindings.Plug(e.ref[e.pos+1])

	if _, ok := plugged.Value.(ast.Var); ok {
		// Note: we might have additional opportunity to optimize here, if we consider that ground values
		// right of e.pos could create a smaller eval "scope" through ref bi-unification before evaluating rules.
		hint.full = true
		hint.key = e.plugged[:e.pos+1]
		e.e.instr.counterIncr(evalOpVirtualCacheMiss)
		return hint, nil
	}

	m := maxRefLength(e.ir.Rules, len(e.ref))

	// Creating the hint key by walking the ref and plugging vars until we hit a non-ground term.
	// Any ground term right of this point will affect the scope of evaluation by ref unification,
	// so we create a virtual-cache scope key to qualify the result stored in the cache.
	//
	// E.g. given the following rule:
	//
	//   package example
	//
	//   a[x][y][z] := x + y + z if {
	//     some x in [1, 2]
	//     some y in [3, 4]
	//     some z in [5, 6]
	//   }
	//
	// and the following ref (1):
	//
	//   data.example.a[1][_][5]
	//
	// then the hint key will be:
	//
	//   data.example.a[1][<_,5>]
	//
	// where <_,5> is the scope of the pre-eval unification.
	// This part does not contribute to the "location" of the cached data.
	//
	// The following ref (2):
	//
	//   data.example.a[1][_][6]
	//
	// will produce the same hint key "location" 'data.example.a[1]', but a different scope component
	// '<_,6>', which will create a different entry in the cache.
	scoping := false
	hintKeyEnd := 0
	for i := e.pos + 1; i < m; i++ {
		plugged = e.bindings.Plug(e.ref[i])

		if plugged.IsGround() && !scoping {
			hintKeyEnd = i
			hint.key = append(e.plugged[:i], plugged)
		} else {
			scoping = true
			hl := len(hint.key)
			if hl == 0 {
				break
			}
			if scope, ok := hint.key[hl-1].Value.(vcKeyScope); ok {
				scope.Ref = append(scope.Ref, plugged)
				hint.key[len(hint.key)-1] = ast.NewTerm(scope)
			} else {
				scope = vcKeyScope{}
				scope.Ref = append(scope.Ref, plugged)
				hint.key = append(hint.key, ast.NewTerm(scope))
			}
		}

		if cached, _ := e.e.virtualCache.Get(hint.key); cached != nil {
			e.e.instr.counterIncr(evalOpVirtualCacheHit)
			hint.hit = true
			return hint, e.evalTerm(iter, hintKeyEnd+1, cached, e.bindings)
		}
	}

	if hl := len(hint.key); hl > 0 {
		if scope, ok := hint.key[hl-1].Value.(vcKeyScope); ok {
			scope = scope.reduce()
			if scope.empty() {
				hint.key = hint.key[:hl-1]
			} else {
				hint.key[hl-1].Value = scope
			}
		}
	}

	e.e.instr.counterIncr(evalOpVirtualCacheMiss)

	return hint, nil
}

// vcKeyScope represents the scoping that pre-rule-eval ref unification imposes on a virtual cache entry.
type vcKeyScope struct {
	ast.Ref
}

func (q vcKeyScope) Compare(other ast.Value) int {
	if q2, ok := other.(vcKeyScope); ok {
		r1 := q.Ref
		r2 := q2.Ref
		if len(r1) != len(r2) {
			return -1
		}

		for i := range r1 {
			_, v1IsVar := r1[i].Value.(ast.Var)
			_, v2IsVar := r2[i].Value.(ast.Var)
			if v1IsVar && v2IsVar {
				continue
			}
			if r1[i].Value.Compare(r2[i].Value) != 0 {
				return -1
			}
		}

		return 0
	}
	return 1
}

func (vcKeyScope) Find(ast.Ref) (ast.Value, error) {
	return nil, nil
}

func (q vcKeyScope) Hash() int {
	var hash int
	for _, v := range q.Ref {
		if _, ok := v.Value.(ast.Var); ok {
			// all vars are equal
			hash++
		} else {
			hash += v.Value.Hash()
		}
	}
	return hash
}

func (vcKeyScope) IsGround() bool {
	return false
}

func (q vcKeyScope) String() string {
	buf := make([]string, 0, len(q.Ref))
	for _, t := range q.Ref {
		if _, ok := t.Value.(ast.Var); ok {
			buf = append(buf, "_")
		} else {
			buf = append(buf, t.String())
		}
	}
	return fmt.Sprintf("<%s>", strings.Join(buf, ","))
}

// reduce removes vars from the tail of the ref.
func (q vcKeyScope) reduce() vcKeyScope {
	ref := q.Ref.Copy()
	var i int
	for i = len(q.Ref) - 1; i >= 0; i-- {
		if _, ok := q.Ref[i].Value.(ast.Var); !ok {
			break
		}
	}
	ref = ref[:i+1]
	return vcKeyScope{ref}
}

func (q vcKeyScope) empty() bool {
	return len(q.Ref) == 0
}

func getNestedObject(ref ast.Ref, rootObj *ast.Object, b *bindings, l *ast.Location) (*ast.Object, error) {
	current := rootObj
	for _, term := range ref {
		key := b.Plug(term)
		if child := (*current).Get(key); child != nil {
			if val, ok := child.Value.(ast.Object); ok {
				current = &val
			} else {
				return nil, objectDocKeyConflictErr(l)
			}
		} else {
			child := ast.NewObject()
			(*current).Insert(key, ast.NewTerm(child))
			current = &child
		}
	}

	return current, nil
}

func hasCollisions(path ast.Ref, visitedRefs *[]ast.Ref, b *bindings) bool {
	collisionPathTerm := b.Plug(ast.NewTerm(path))
	collisionPath := collisionPathTerm.Value.(ast.Ref)
	for _, c := range *visitedRefs {
		if collisionPath.HasPrefix(c) && !collisionPath.Equal(c) {
			return true
		}
	}
	*visitedRefs = append(*visitedRefs, collisionPath)
	return false
}

func (e evalVirtualPartial) reduce(rule *ast.Rule, b *bindings, result *ast.Term, visitedRefs *[]ast.Ref) (*ast.Term, bool, error) {

	var exists bool
	head := rule.Head

	switch v := result.Value.(type) {
	case ast.Set:
		key := b.Plug(head.Key)
		exists = v.Contains(key)
		v.Add(key)
	case ast.Object:
		// data.p.q[r].s.t := 42 {...}
		//         |----|-|
		//          ^    ^
		//          |    leafKey
		//          objPath
		fullPath := rule.Ref()

		collisionPath := fullPath[e.pos+1:]
		if hasCollisions(collisionPath, visitedRefs, b) {
			return nil, false, objectDocKeyConflictErr(head.Location)
		}

		objPath := fullPath[e.pos+1 : len(fullPath)-1] // the portion of the ref that generates nested objects
		leafKey := b.Plug(fullPath[len(fullPath)-1])   // the portion of the ref that is the deepest nested key for the value

		leafObj, err := getNestedObject(objPath, &v, b, head.Location)
		if err != nil {
			return nil, false, err
		}

		if kind := head.RuleKind(); kind == ast.SingleValue {
			// We're inserting into an object
			val := b.Plug(head.Value) // head.Value instance is shared between rule enumerations;but this is ok, as we don't allow rules to modify each others values.

			if curr := (*leafObj).Get(leafKey); curr != nil {
				if !curr.Equal(val) {
					return nil, false, objectDocKeyConflictErr(head.Location)
				}
				exists = true
			} else {
				(*leafObj).Insert(leafKey, val)
			}
		} else {
			// We're inserting into a set
			var set *ast.Set
			if leaf := (*leafObj).Get(leafKey); leaf != nil {
				if s, ok := leaf.Value.(ast.Set); ok {
					set = &s
				} else {
					return nil, false, objectDocKeyConflictErr(head.Location)
				}
			} else {
				s := ast.NewSet()
				(*leafObj).Insert(leafKey, ast.NewTerm(s))
				set = &s
			}

			key := b.Plug(head.Key)
			exists = (*set).Contains(key)
			(*set).Add(key)
		}
	}

	return result, exists, nil
}

type evalVirtualComplete struct {
	e         *eval
	ir        *ast.IndexResult
	bindings  *bindings
	rterm     *ast.Term
	rbindings *bindings
	ref       ast.Ref
	plugged   ast.Ref
	pos       int
}

func (e evalVirtualComplete) eval(iter unifyIterator) error {

	if e.ir.Empty() {
		return nil
	}

	// When evaluating the full extent, skip functions.
	if len(e.ir.Rules) > 0 && len(e.ir.Rules[0].Head.Args) > 0 ||
		e.ir.Default != nil && len(e.ir.Default.Head.Args) > 0 {
		return nil
	}

	if !e.e.unknownRef(e.ref, e.bindings) {
		return e.evalValue(iter, e.ir.EarlyExit)
	}

	var generateSupport bool

	if e.ir.Default != nil {
		// If inlining has been disabled for the rterm, and the default rule has a 'false' result value,
		// the default value is inconsequential, and support does not need to be generated.
		if !(e.ir.Default.Head.Value.Equal(ast.InternedTerm(false)) && e.e.inliningControl.Disabled(e.rterm.Value, false)) {
			// If the other term is not constant OR it's equal to the default value, then
			// a support rule must be produced as the default value _may_ be required. On
			// the other hand, if the other term is constant (i.e., it does not require
			// evaluation) and it differs from the default value then the default value is
			// _not_ required, so partially evaluate the rule normally.
			rterm := e.rbindings.Plug(e.rterm)
			generateSupport = !ast.IsConstant(rterm.Value) || e.ir.Default.Head.Value.Equal(rterm)
		}
	}

	if generateSupport || e.e.inliningControl.shallow || e.e.inliningControl.Disabled(e.plugged[:e.pos+1], false) {
		return e.partialEvalSupport(iter)
	}

	return e.partialEval(iter)
}

func (e evalVirtualComplete) evalValue(iter unifyIterator, findOne bool) error {
	cached, undefined := e.e.virtualCache.Get(e.plugged[:e.pos+1])
	if undefined {
		e.e.instr.counterIncr(evalOpVirtualCacheHit)
		return nil
	}

	// a cached result won't generate any EE from evaluating the rule, so we exempt it from EE suppression to not
	// drop EE generated by the caller (through `iter` invocation).
	if cached != nil {
		e.e.instr.counterIncr(evalOpVirtualCacheHit)
		return e.evalTerm(iter, cached, e.bindings)
	}

	return withSuppressEarlyExit(func() error {
		e.e.instr.counterIncr(evalOpVirtualCacheMiss)

		var prev *ast.Term
		var deferredEe *deferredEarlyExitError

		for _, rule := range e.ir.Rules {
			next, err := e.evalValueRule(iter, rule, prev, findOne)
			if err != nil {
				if dee, ok := err.(*deferredEarlyExitError); ok {
					if deferredEe == nil {
						deferredEe = dee
					}
				} else {
					return err
				}
			}
			if next == nil {
				for _, erule := range e.ir.Else[rule] {
					next, err = e.evalValueRule(iter, erule, prev, findOne)
					if err != nil {
						if dee, ok := err.(*deferredEarlyExitError); ok {
							if deferredEe == nil {
								deferredEe = dee
							}
						} else {
							return err
						}
					}
					if next != nil {
						break
					}
				}
			}
			if next != nil {
				prev = next
			}
		}

		if e.ir.Default != nil && prev == nil {
			_, err := e.evalValueRule(iter, e.ir.Default, prev, findOne)
			return err
		}

		if prev == nil {
			e.e.virtualCache.Put(e.plugged[:e.pos+1], nil)
		}

		if deferredEe != nil {
			return deferredEe
		}

		return nil
	})
}

func (e evalVirtualComplete) evalValueRule(iter unifyIterator, rule *ast.Rule, prev *ast.Term, findOne bool) (*ast.Term, error) {
	child := evalPool.Get()
	defer evalPool.Put(child)

	e.e.child(rule.Body, child)
	child.findOne = findOne
	child.traceEnter(rule)
	var result *ast.Term
	err := child.eval(func(child *eval) error {
		child.traceExit(rule)

		result = child.bindings.Plug(rule.Head.Value)

		if prev != nil {
			if ast.Compare(result, prev) != 0 {
				return completeDocConflictErr(rule.Location)
			}
			child.traceRedo(rule)
			return nil
		}

		prev = result
		e.e.virtualCache.Put(e.plugged[:e.pos+1], result)

		term, termbindings := child.bindings.apply(rule.Head.Value)
		err := e.evalTerm(iter, term, termbindings)
		if err != nil {
			return err
		}

		// TODO: trace redo if EE-err && !findOne(?)
		child.traceRedo(rule)
		return nil
	})

	return result, err
}

func (e evalVirtualComplete) partialEval(iter unifyIterator) error {
	child := evalPool.Get()
	defer evalPool.Put(child)

	for _, rule := range e.ir.Rules {
		e.e.child(rule.Body, child)
		child.traceEnter(rule)

		err := child.eval(func(child *eval) error {
			child.traceExit(rule)
			term, termbindings := child.bindings.apply(rule.Head.Value)

			err := e.evalTerm(iter, term, termbindings)
			if err != nil {
				return err
			}

			child.traceRedo(rule)
			return nil
		})

		if err != nil {
			return err
		}
	}

	return nil
}

func (e evalVirtualComplete) partialEvalSupport(iter unifyIterator) error {

	path := e.e.namespaceRef(e.plugged[:e.pos+1])
	term := ast.NewTerm(e.e.namespaceRef(e.ref))

	var defined bool

	if e.e.saveSupport.Exists(path) {
		defined = true
	} else {
		for i := range e.ir.Rules {
			ok, err := e.partialEvalSupportRule(e.ir.Rules[i], path)
			if err != nil {
				return err
			}
			if ok {
				defined = true
			}
		}

		if e.ir.Default != nil {
			ok, err := e.partialEvalSupportRule(e.ir.Default, path)
			if err != nil {
				return err
			}
			if ok {
				defined = true
			}
		}
	}

	if !defined {
		return nil
	}

	return e.e.saveUnify(term, e.rterm, e.bindings, e.rbindings, iter)
}

func (e evalVirtualComplete) partialEvalSupportRule(rule *ast.Rule, path ast.Ref) (bool, error) {
	child := evalPool.Get()
	defer evalPool.Put(child)

	e.e.child(rule.Body, child)
	child.traceEnter(rule)

	e.e.saveStack.PushQuery(nil)
	var defined bool

	err := child.eval(func(child *eval) error {
		child.traceExit(rule)
		defined = true

		current := e.e.saveStack.PopQuery()
		plugged := current.Plug(e.e.caller.bindings)
		// Skip this rule body if it fails to type-check.
		// Type-checking failure means the rule body will never succeed.
		if e.e.compiler.PassesTypeCheck(plugged) {
			pkg, ruleRef := splitPackageAndRule(path)
			head := ast.RefHead(ruleRef, child.bindings.PlugNamespaced(rule.Head.Value, e.e.caller.bindings))

			if !e.e.inliningControl.shallow {
				cp := copypropagation.New(head.Vars()).
					WithEnsureNonEmptyBody(true).
					WithCompiler(e.e.compiler)
				plugged = applyCopyPropagation(cp, e.e.instr, plugged)
			}

			e.e.saveSupport.InsertByPkg(pkg, &ast.Rule{
				Head:    head,
				Body:    plugged,
				Default: rule.Default,
			})
		}
		child.traceRedo(rule)
		e.e.saveStack.PushQuery(current)
		return nil
	})
	e.e.saveStack.PopQuery()
	return defined, err
}

func (e evalVirtualComplete) evalTerm(iter unifyIterator, term *ast.Term, termbindings *bindings) error {
	eval := evalTerm{
		e:            e.e,
		ref:          e.ref,
		pos:          e.pos + 1,
		bindings:     e.bindings,
		term:         term,
		termbindings: termbindings,
		rterm:        e.rterm,
		rbindings:    e.rbindings,
	}
	return eval.eval(iter)
}

type evalTerm struct {
	e            *eval
	bindings     *bindings
	term         *ast.Term
	termbindings *bindings
	rterm        *ast.Term
	rbindings    *bindings
	ref          ast.Ref
	pos          int
}

func (e evalTerm) eval(iter unifyIterator) error {

	if len(e.ref) == e.pos {
		return e.e.biunify(e.term, e.rterm, e.termbindings, e.rbindings, iter)
	}

	if e.e.saveSet.Contains(e.term, e.termbindings) {
		return e.save(iter)
	}

	plugged := e.bindings.Plug(e.ref[e.pos])

	if plugged.IsGround() {
		return e.next(iter, plugged)
	}

	return e.enumerate(iter)
}

func (e evalTerm) next(iter unifyIterator, plugged *ast.Term) error {

	term, bindings := e.get(plugged)
	if term == nil {
		return nil
	}

	cpy := e
	cpy.term = term
	cpy.termbindings = bindings
	cpy.pos++
	return cpy.eval(iter)
}

func (e evalTerm) enumerate(iter unifyIterator) error {
	var deferredEe *deferredEarlyExitError
	handleErr := func(err error) error {
		var dee *deferredEarlyExitError
		if errors.As(err, &dee) {
			if deferredEe == nil {
				deferredEe = dee
			}
			return nil
		}
		return err
	}

	switch v := e.term.Value.(type) {
	case *ast.Array:
		// Note(anders):
		// For this case (e.g. input.foo[_]), we can avoid the (quite expensive) overhead of a callback
		// function literal escaping to the heap in each iteration by inlining the biunification logic,
		// meaning a 10x reduction in both the number of allocations made as well as the memory consumed.
		// It is possible that such inlining could be done for the set/object cases as well, and that's
		// worth looking into later, as I imagine set iteration in particular would be an even greater
		// win across most policies. Those cases are however much more complex, as we need to deal with
		// any type on either side, not just int/var as is the case here.
		for i := range v.Len() {
			a := ast.InternedTerm(i)
			b := e.ref[e.pos]

			if _, ok := b.Value.(ast.Var); ok {
				if e.e.traceEnabled {
					e.e.traceUnify(a, b)
				}
				var undo undo
				b, e.bindings = e.bindings.apply(b)
				e.bindings.bind(b, a, e.bindings, &undo)

				err := e.next(iter, a)
				undo.Undo()
				if err != nil {
					if err := handleErr(err); err != nil {
						return err
					}
				}
			}
		}
	case ast.Object:
		for _, k := range v.Keys() {
			err := e.e.biunify(k, e.ref[e.pos], e.termbindings, e.bindings, func() error {
				return e.next(iter, e.termbindings.Plug(k))
			})
			if err != nil {
				if err := handleErr(err); err != nil {
					return err
				}
			}
		}
	case ast.Set:
		for _, elem := range v.Slice() {
			err := e.e.biunify(elem, e.ref[e.pos], e.termbindings, e.bindings, func() error {
				return e.next(iter, e.termbindings.Plug(elem))
			})
			if err != nil {
				if err := handleErr(err); err != nil {
					return err
				}
			}
		}
	}

	if deferredEe != nil {
		return deferredEe
	}
	return nil
}

func (e evalTerm) get(plugged *ast.Term) (*ast.Term, *bindings) {
	switch v := e.term.Value.(type) {
	case ast.Set:
		if v.IsGround() {
			if v.Contains(plugged) {
				return e.termbindings.apply(plugged)
			}
		} else {
			var t *ast.Term
			var b *bindings
			stop := v.Until(func(elem *ast.Term) bool {
				if e.termbindings.Plug(elem).Equal(plugged) {
					t, b = e.termbindings.apply(plugged)
					return true
				}
				return false
			})
			if stop {
				return t, b
			}
		}
	case ast.Object:
		if v.IsGround() {
			term := v.Get(plugged)
			if term != nil {
				return e.termbindings.apply(term)
			}
		} else {
			var t *ast.Term
			var b *bindings
			stop := v.Until(func(k, v *ast.Term) bool {
				if e.termbindings.Plug(k).Equal(plugged) {
					t, b = e.termbindings.apply(v)
					return true
				}
				return false
			})
			if stop {
				return t, b
			}
		}
	case *ast.Array:
		term := v.Get(plugged)
		if term != nil {
			return e.termbindings.apply(term)
		}
	}
	return nil, nil
}

func (e evalTerm) save(iter unifyIterator) error {

	v := e.e.generateVar(fmt.Sprintf("ref_%d", e.e.genvarid))
	e.e.genvarid++

	return e.e.biunify(e.term, v, e.termbindings, e.bindings, func() error {

		suffix := e.ref[e.pos:]
		ref := make(ast.Ref, len(suffix)+1)
		ref[0] = v
		copy(ref[1:], suffix)

		return e.e.biunify(ast.NewTerm(ref), e.rterm, e.bindings, e.rbindings, iter)
	})

}

type evalEvery struct {
	*ast.Every
	e    *eval
	expr *ast.Expr
}

func (e evalEvery) eval(iter unifyIterator) error {
	// unknowns in domain or body: save the expression, PE its body
	if e.e.unknown(e.Domain, e.e.bindings) || e.e.unknown(e.Body, e.e.bindings) {
		return e.save(iter)
	}

	if pd := e.e.bindings.Plug(e.Domain); pd != nil {
		if !isIterableValue(pd.Value) {
			e.e.traceFail(e.expr)
			return nil
		}
	}

	generator := ast.NewBody(
		ast.Equality.Expr(
			ast.RefTerm(e.Domain, e.Key).SetLocation(e.Domain.Location),
			e.Value,
		).SetLocation(e.Domain.Location),
	)

	domain := evalPool.Get()
	defer evalPool.Put(domain)

	e.e.closure(generator, domain)

	all := true // all generator evaluations yield one successful body evaluation

	domain.traceEnter(e.expr)

	err := domain.eval(func(child *eval) error {
		if !all {
			// NOTE(sr): Is this good enough? We don't have a "fail EE".
			// This would do extra work, like iterating needlessly if domain was a large array.
			return nil
		}

		body := evalPool.Get()
		defer evalPool.Put(body)

		child.closure(e.Body, body)
		body.findOne = true
		body.traceEnter(e.Body)
		done := false
		err := body.eval(func(*eval) error {
			body.traceExit(e.Body)
			done = true
			body.traceRedo(e.Body)
			return nil
		})
		if !done {
			all = false
		}

		child.traceRedo(e.expr)

		// We don't want to abort the generator domain enumeration with EE.
		return suppressEarlyExit(err)
	})

	if err != nil {
		return err
	}

	if all {
		err := iter()
		domain.traceExit(e.expr)
		return err
	}
	domain.traceFail(e.expr)
	return nil
}

// isIterableValue returns true if the AST value is an iterable type.
func isIterableValue(x ast.Value) bool {
	switch x.(type) {
	case *ast.Array, ast.Object, ast.Set:
		return true
	}
	return false
}

func (e *evalEvery) save(iter unifyIterator) error {
	return e.e.saveExpr(e.plug(e.expr), e.e.bindings, iter)
}

func (e *evalEvery) plug(expr *ast.Expr) *ast.Expr {
	cpy := expr.Copy()
	every := cpy.Terms.(*ast.Every)
	for i := range every.Body {
		switch t := every.Body[i].Terms.(type) {
		case *ast.Term:
			every.Body[i].Terms = e.e.bindings.PlugNamespaced(t, e.e.caller.bindings)
		case []*ast.Term:
			for j := 1; j < len(t); j++ { // don't plug operator, t[0]
				t[j] = e.e.bindings.PlugNamespaced(t[j], e.e.caller.bindings)
			}
		case *ast.Every:
			every.Body[i] = e.plug(every.Body[i])
		}
	}

	every.Key = e.e.bindings.PlugNamespaced(every.Key, e.e.caller.bindings)
	every.Value = e.e.bindings.PlugNamespaced(every.Value, e.e.caller.bindings)
	every.Domain = e.e.bindings.PlugNamespaced(every.Domain, e.e.caller.bindings)
	cpy.Terms = every
	return cpy
}

func (e *eval) comprehensionIndex(term *ast.Term) *ast.ComprehensionIndex {
	if e.queryCompiler != nil {
		return e.queryCompiler.ComprehensionIndex(term)
	}
	return e.compiler.ComprehensionIndex(term)
}

func (e *eval) namespaceRef(ref ast.Ref) ast.Ref {
	if e.skipSaveNamespace {
		return ref.Copy()
	}
	return ref.Insert(e.saveNamespace, 1)
}

type savePair struct {
	term *ast.Term
	b    *bindings
}

func getSavePairsFromExpr(declArgsLen int, x *ast.Expr, b *bindings, result []savePair) []savePair {
	switch terms := x.Terms.(type) {
	case *ast.Term:
		return getSavePairsFromTerm(terms, b, result)
	case []*ast.Term:
		if x.IsEquality() {
			return getSavePairsFromTerm(terms[2], b, getSavePairsFromTerm(terms[1], b, result))
		}
		if declArgsLen == len(terms)-2 {
			return getSavePairsFromTerm(terms[len(terms)-1], b, result)
		}
	}
	return result
}

func getSavePairsFromTerm(x *ast.Term, b *bindings, result []savePair) []savePair {
	if _, ok := x.Value.(ast.Var); ok {
		result = append(result, savePair{x, b})
		return result
	}
	vis := ast.NewVarVisitor().WithParams(ast.VarVisitorParams{
		SkipClosures: true,
		SkipRefHead:  true,
	})
	vis.Walk(x)
	for v := range vis.Vars() {
		y, next := b.apply(ast.NewTerm(v))
		result = getSavePairsFromTerm(y, next, result)
	}
	return result
}

func applyCopyPropagation(p *copypropagation.CopyPropagator, instr *Instrumentation, body ast.Body) ast.Body {
	instr.startTimer(partialOpCopyPropagation)
	result := p.Apply(body)
	instr.stopTimer(partialOpCopyPropagation)
	return result
}

func nonGroundKey(k, _ *ast.Term) bool {
	return !k.IsGround()
}

func nonGroundKeys(a ast.Object) bool {
	return a.Until(nonGroundKey)
}

func plugKeys(a ast.Object, b *bindings) ast.Object {
	plugged, _ := a.Map(func(k, v *ast.Term) (*ast.Term, *ast.Term, error) {
		return b.Plug(k), v, nil
	})
	return plugged
}

func canInlineNegation(safe ast.VarSet, queries []ast.Body) bool {

	size := 1
	vis := newNestedCheckVisitor()

	for _, query := range queries {
		size *= len(query)
		for _, expr := range query {
			if containsNestedRefOrCall(vis, expr) {
				// Expressions containing nested refs or calls cannot be trivially negated
				// because the semantics would change. For example, the complement of `not f(input.x)`
				// is _not_ `f(input.x)`--it is `not input.x` OR `f(input.x)`.
				//
				// NOTE(tsandall): Since this would require the complement function to undo the
				// copy propagation optimization, just bail out here. If this becomes a problem
				// in the future, we can handle more cases.
				return false
			}
			if !expr.Negated {
				// Positive expressions containing variables cannot be trivially negated
				// because they become unsafe (e.g., "x = 1" negated is "not x = 1" making x
				// unsafe.) We check if the vars in the expr are already safe.
				vis := ast.NewVarVisitor().WithParams(ast.VarVisitorParams{
					SkipRefCallHead: true,
					SkipClosures:    true,
				})
				vis.Walk(expr)
				if vis.Vars().Diff(safe).DiffCount(ast.ReservedVars) > 0 {
					return false
				}
			}
		}
	}

	// NOTE(tsandall): this limit is arbitrary–it's only in place to prevent the
	// partial evaluation result from blowing up. In the future, we could make this
	// configurable or do something more clever.
	return size <= 16
}

type nestedCheckVisitor struct {
	vis   *ast.GenericVisitor
	found bool
}

func newNestedCheckVisitor() *nestedCheckVisitor {
	v := &nestedCheckVisitor{}
	v.vis = ast.NewGenericVisitor(v.visit)
	return v
}

func (v *nestedCheckVisitor) visit(x any) bool {
	switch x.(type) {
	case ast.Ref, ast.Call:
		v.found = true
	}
	return v.found
}

func containsNestedRefOrCall(vis *nestedCheckVisitor, expr *ast.Expr) bool {

	if expr.IsEquality() {
		for _, term := range expr.Operands() {
			if containsNestedRefOrCallInTerm(vis, term) {
				return true
			}
		}
		return false
	}

	if expr.IsCall() {
		for _, term := range expr.Operands() {
			vis.vis.Walk(term)
			if vis.found {
				return true
			}
		}
		return false
	}

	return containsNestedRefOrCallInTerm(vis, expr.Terms.(*ast.Term))
}

func containsNestedRefOrCallInTerm(vis *nestedCheckVisitor, term *ast.Term) bool {
	switch v := term.Value.(type) {
	case ast.Ref:
		for i := 1; i < len(v); i++ {
			vis.vis.Walk(v[i])
			if vis.found {
				return true
			}
		}
		return false
	default:
		vis.vis.Walk(v)
		if vis.found {
			return true
		}
		return false
	}
}

func complementedCartesianProduct(queries []ast.Body, idx int, curr ast.Body, iter func(ast.Body) error) error {
	if idx == len(queries) {
		return iter(curr)
	}
	for _, expr := range queries[idx] {
		curr = append(curr, expr.Complement())
		if err := complementedCartesianProduct(queries, idx+1, curr, iter); err != nil {
			return err
		}
		curr = curr[:len(curr)-1]
	}
	return nil
}

func isInputRef(term *ast.Term) bool {
	if ref, ok := term.Value.(ast.Ref); ok {
		if ref.HasPrefix(ast.InputRootRef) {
			return true
		}
	}
	return false
}

func isDataRef(term *ast.Term) bool {
	if ref, ok := term.Value.(ast.Ref); ok {
		if ref.HasPrefix(ast.DefaultRootRef) {
			return true
		}
	}
	return false
}

func isOtherRef(term *ast.Term) bool {
	ref, ok := term.Value.(ast.Ref)
	if !ok {
		panic("unreachable")
	}
	return !ref.HasPrefix(ast.DefaultRootRef) && !ref.HasPrefix(ast.InputRootRef)
}

func isFunction(env *ast.TypeEnv, ref any) bool {
	var r ast.Ref
	switch v := ref.(type) {
	case ast.Ref:
		r = v
	case *ast.Term:
		return isFunction(env, v.Value)
	case ast.Value:
		return false
	default:
		panic("expected ast.Value or *ast.Term")
	}
	_, ok := env.Get(r).(*types.Function)
	return ok
}

func merge(a, b ast.Value) (ast.Value, bool) {
	aObj, ok1 := a.(ast.Object)
	bObj, ok2 := b.(ast.Object)

	if ok1 && ok2 {
		return mergeObjects(aObj, bObj)
	}

	// nothing to merge, a wins
	return a, true
}

// mergeObjects returns a new Object containing the non-overlapping keys of
// the objA and objB. If there are overlapping keys between objA and objB,
// the values of associated with the keys are merged. Only
// objects can be merged with other objects. If the values cannot be merged,
// objB value will be overwritten by objA value.
func mergeObjects(objA, objB ast.Object) (result ast.Object, ok bool) {
	result = ast.NewObject()
	stop := objA.Until(func(k, v *ast.Term) bool {
		if v2 := objB.Get(k); v2 == nil {
			result.Insert(k, v)
		} else {
			obj1, ok1 := v.Value.(ast.Object)
			obj2, ok2 := v2.Value.(ast.Object)

			if !ok1 || !ok2 {
				result.Insert(k, v)
				return false
			}
			obj3, ok := mergeObjects(obj1, obj2)
			if !ok {
				return true
			}
			result.Insert(k, ast.NewTerm(obj3))
		}
		return false
	})
	if stop {
		return nil, false
	}
	objB.Foreach(func(k, v *ast.Term) {
		if v2 := objA.Get(k); v2 == nil {
			result.Insert(k, v)
		}
	})
	return result, true
}

func refContainsNonScalar(ref ast.Ref) bool {
	for _, term := range ref[1:] {
		if !ast.IsScalar(term.Value) {
			return true
		}
	}
	return false
}

func suppressEarlyExit(err error) error {
	if ee, ok := err.(*earlyExitError); ok {
		return ee.prev
	} else if oee, ok := err.(*deferredEarlyExitError); ok {
		return oee.prev
	}
	return err
}

func withSuppressEarlyExit(f func() error) error {
	if err := f(); err != nil {
		return suppressEarlyExit(err)
	}
	return nil
}

func (e *eval) updateSavedMocks(withs []*ast.With) []*ast.With {
	ret := make([]*ast.With, 0, len(withs))
	for _, w := range withs {
		if isOtherRef(w.Target) || isFunction(e.compiler.TypeEnv, w.Target) {
			continue
		}
		ret = append(ret, w.Copy())
	}
	return ret
}
