package startosis_engine

import (
	"context"
	"fmt"
	"github.com/kurtosis-tech/kurtosis/api/golang/core/kurtosis_core_rpc_api_bindings"
	"github.com/kurtosis-tech/kurtosis/container-engine-lib/lib/backend_interface/objects/image_download_mode"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/service_network"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/builtins"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/builtins/print_builtin"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/enclave_plan_persistence"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/enclave_structure"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/instructions_plan"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/instructions_plan/resolver"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/interpretation_time_value_store"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/kurtosis_instruction/plan_module"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/kurtosis_types"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/package_io"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/runtime_value_store"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/startosis_constants"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/startosis_errors"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/startosis_packages"
	"github.com/kurtosis-tech/kurtosis/core/server/api_container/server/startosis_engine/startosis_packages/git_package_content_provider"
	"github.com/sirupsen/logrus"
	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
	"path"
	"strings"
	"sync"
)

const (
	multipleInterpretationErrorMsg = "Multiple errors caught interpreting the Starlark script. Listing each of them below."
	evaluationErrorPrefix          = "Evaluation error: "

	skipImportInstructionInStacktraceValue = "import_module"

	runFunctionName = "run"

	paramsRequiredForArgs        = 2
	minimumParamsRequiredForPlan = 1

	planParamIndex         = 0
	planParamName          = "plan"
	argsParamIndex         = 1
	argsParamName          = "args"
	unexpectedArgNameError = "Expected argument at index '%v' of run function to be called '%v' got '%v' "
)

var (
	noKwargs []starlark.Tuple
)

type StartosisInterpreter struct {
	// This is mutex protected as interpreting two different scripts in parallel could potentially cause
	// problems with moduleContentProvider. Fixing this is quite complicated, which we decided not to do.
	mutex          *sync.Mutex
	serviceNetwork service_network.ServiceNetwork
	recipeExecutor *runtime_value_store.RuntimeValueStore
	// TODO AUTH there will be a leak here in case people with different repo visibility access a module
	packageContentProvider       startosis_packages.PackageContentProvider
	starlarkValueSerde           *kurtosis_types.StarlarkValueSerde
	enclaveEnvVars               string
	interpretationTimeValueStore *interpretation_time_value_store.InterpretationTimeValueStore
	// This is a function that allows the consumer of the interpreter to adjust the default builtins.
	// It is useful when external libraries or helpers need to be plugged in to kurtosis,
	// for example when running unit tests using the starlarktest package
	processBuiltins StartosisInterpreterBuiltinsProcessor
}

// StartosisInterpreterBuiltinsProcessor is a builtins transformer function
type StartosisInterpreterBuiltinsProcessor func(thread *starlark.Thread, predeclared starlark.StringDict) starlark.StringDict

type SerializedInterpretationOutput string

func NewStartosisInterpreter(serviceNetwork service_network.ServiceNetwork, packageContentProvider startosis_packages.PackageContentProvider, runtimeValueStore *runtime_value_store.RuntimeValueStore, starlarkValueSerde *kurtosis_types.StarlarkValueSerde, enclaveVarEnvs string, interpretationTimeValueStore *interpretation_time_value_store.InterpretationTimeValueStore) *StartosisInterpreter {
	return NewStartosisInterpreterWithBuiltinsProcessor(serviceNetwork, packageContentProvider, runtimeValueStore, starlarkValueSerde, enclaveVarEnvs, interpretationTimeValueStore, nil)
}

func NewStartosisInterpreterWithBuiltinsProcessor(serviceNetwork service_network.ServiceNetwork, packageContentProvider startosis_packages.PackageContentProvider, runtimeValueStore *runtime_value_store.RuntimeValueStore, starlarkValueSerde *kurtosis_types.StarlarkValueSerde, enclaveVarEnvs string, interpretationTimeValueStore *interpretation_time_value_store.InterpretationTimeValueStore, processBuiltins StartosisInterpreterBuiltinsProcessor) *StartosisInterpreter {
	return &StartosisInterpreter{
		mutex:                        &sync.Mutex{},
		serviceNetwork:               serviceNetwork,
		recipeExecutor:               runtimeValueStore,
		packageContentProvider:       packageContentProvider,
		enclaveEnvVars:               enclaveVarEnvs,
		starlarkValueSerde:           starlarkValueSerde,
		interpretationTimeValueStore: interpretationTimeValueStore,
		processBuiltins:              processBuiltins,
	}
}

// InterpretAndOptimizePlan is an evolution of the Interpret function which takes into account the current enclave
// plan when  interpreting the Starlark package in order to not re-run the instructions that have alrady been run
// inside the enclave.
// What it does is that is calls the Interpret function with a mask that swap certain instructions with the ones from
// the current enclave plan abd see what is affected by those swaps. If the new plan produced by the interpretation
// with the swaps is correct, then it returns it. See detailed inline comments for more details on the implementation
//
// Note: the plan generated by this function is necessarily a SUBSET of the currentEnclavePlan passed as a parameter
func (interpreter *StartosisInterpreter) InterpretAndOptimizePlan(
	ctx context.Context,
	packageId string,
	packageReplaceOptions map[string]string,
	mainFunctionName string,
	relativePathtoMainFile string,
	serializedStarlark string,
	serializedJsonParams string,
	nonBlockingMode bool,
	currentEnclavePlan *enclave_plan_persistence.EnclavePlan,
	imageDownloadMode image_download_mode.ImageDownloadMode,
) (string, *instructions_plan.InstructionsPlan, *kurtosis_core_rpc_api_bindings.StarlarkInterpretationError) {

	if interpretationErr := interpreter.packageContentProvider.CloneReplacedPackagesIfNeeded(packageReplaceOptions); interpretationErr != nil {
		return "", nil, interpretationErr.ToAPIType()
	}

	// run interpretation with no mask at all to generate the list of instructions as if the enclave was empty
	enclaveComponents := enclave_structure.NewEnclaveComponents()
	emptyPlanInstructionsMask := resolver.NewInstructionsPlanMask(0)
	naiveInstructionsPlanSerializedScriptOutput, naiveInstructionsPlan, interpretationErrorApi := interpreter.Interpret(ctx, packageId, mainFunctionName, packageReplaceOptions, relativePathtoMainFile, serializedStarlark, serializedJsonParams, nonBlockingMode, enclaveComponents, emptyPlanInstructionsMask, imageDownloadMode)
	if interpretationErrorApi != nil {
		return startosis_constants.NoOutputObject, nil, interpretationErrorApi
	}

	naiveInstructionsPlanSequence, interpretationErr := naiveInstructionsPlan.GeneratePlan()
	if interpretationErr != nil {
		return startosis_constants.NoOutputObject, nil, interpretationErr.ToAPIType()
	}
	logrus.Debugf("First interpretation of package generated %d instructions", len(naiveInstructionsPlanSequence))

	currentEnclavePlanSequence := currentEnclavePlan.GeneratePlan()
	if interpretationErr != nil {
		return startosis_constants.NoOutputObject, nil, interpretationErr.ToAPIType()
	}
	logrus.Debugf("Current enclave state contains %d instructions", len(currentEnclavePlanSequence))
	logrus.Debugf("Starting iterations to find the best plan to execute given the current state of the enclave")

	// We're going to iterate this way:
	// 1. Find an instruction in the current enclave plan matching the first instruction of the new plan
	// 2. Recopy all instructions prior to the match into the optimized plan
	// 3. Recopy all following instruction from the current enclave plan into an Instructions Plan Mask -> the reason
	//    we're naively recopying all the following instructions, not just the ones that depends on this instruction
	//    is because right now, we don't have the ability to know which instructions depends on which. We assume that
	//    all instructions executed AFTER this one will depend on it, to stay on the safe side
	// 4. Run the interpretation with the mask.
	//     - If it's successful, then we've found the optimized plan
	//     - if it's not successful, then the mask is not compatible with the package. Go back to step 1
	var firstPossibleIndexForMatchingInstruction int
	if currentEnclavePlan.Size() > naiveInstructionsPlan.Size() {
		firstPossibleIndexForMatchingInstruction = currentEnclavePlan.Size() - naiveInstructionsPlan.Size()
	}
	for {
		// initialize an empty optimized plan and an empty the mask
		potentialMask := resolver.NewInstructionsPlanMask(len(naiveInstructionsPlanSequence))
		optimizedPlan := instructions_plan.NewInstructionsPlan()

		// find the index of an instruction in the current enclave plan matching the FIRST instruction of our instructions plan generated by the first interpretation
		matchingInstructionIdx := findFirstEqualInstructionPastIndex(currentEnclavePlanSequence, naiveInstructionsPlanSequence, firstPossibleIndexForMatchingInstruction)
		if matchingInstructionIdx >= 0 {
			logrus.Debugf("Found an instruction in enclave state at index %d which matches the first instruction of the new instructions plan", matchingInstructionIdx)
			// we found a match
			// -> First store that index into the plan so that all instructions prior to this match will be
			// kept in the enclave plan
			logrus.Debugf("Stored index of matching instructions: %d into the new plan. The instructions prior to this index in the enclave plan won't be executed but need to be kept in the enclave plan", matchingInstructionIdx)
			optimizedPlan.SetIndexOfFirstInstruction(matchingInstructionIdx)
			// -> Then recopy all instructions past this match from the enclave state to the mask
			// Those instructions are the instructions that will mask the instructions for the newly submitted plan
			numberOfInstructionCopiedToMask := 0
			for copyIdx := matchingInstructionIdx; copyIdx < len(currentEnclavePlanSequence); copyIdx++ {
				if numberOfInstructionCopiedToMask >= potentialMask.Size() {
					// the mask is already full, can't recopy more instructions, stop here
					break
				}
				potentialMask.InsertAt(numberOfInstructionCopiedToMask, currentEnclavePlanSequence[copyIdx])
				numberOfInstructionCopiedToMask += 1
			}
			logrus.Debugf("Writing %d instruction at the beginning of the plan mask, leaving %d empty at the end", numberOfInstructionCopiedToMask, potentialMask.Size()-numberOfInstructionCopiedToMask)
		} else {
			// We cannot find any more instructions inside the enclave state matching the first instruction of the plan
			optimizedPlan.SetIndexOfFirstInstruction(currentEnclavePlan.Size())
			for _, newPlanInstruction := range naiveInstructionsPlanSequence {
				optimizedPlan.AddScheduledInstruction(newPlanInstruction)
			}
			logrus.Debugf("Exhausted all possibilities. Concatenated the previous enclave plan with the new plan to obtain a %d instructions plan", optimizedPlan.Size())
			return naiveInstructionsPlanSerializedScriptOutput, optimizedPlan, nil
		}

		// Now that we have a potential plan mask, try running interpretation again using this plan mask
		attemptSerializedScriptOutput, attemptInstructionsPlan, interpretationErrorApi := interpreter.Interpret(ctx, packageId, mainFunctionName, packageReplaceOptions, relativePathtoMainFile, serializedStarlark, serializedJsonParams, nonBlockingMode, enclaveComponents, potentialMask, imageDownloadMode)
		if interpretationErrorApi != nil {
			// Note: there's no real reason why this interpretation would fail with an error, given that the package
			// has been interpreted once already (right above). But to be on the safe side, check the error
			logrus.Warnf("Interpreting the package again with the plan mask failed, this is an unexpected error. " +
				"Ignoring this mask")
			firstPossibleIndexForMatchingInstruction += 1
			continue
		}
		if !potentialMask.IsValid() {
			// mask has been marks as invalid by the interpreter, we need to find another one
			logrus.Infof("Plan mask was marked as invalid after the tentative-interpretation returned. Will " +
				"ignore this mask and try to find another one")
			firstPossibleIndexForMatchingInstruction += 1
			continue
		}

		// no error happened, it seems we found a good mask
		// -> recopy all instructions from the interpretation to the optimized plan
		attemptInstructionsPlanSequence, interpretationErr := attemptInstructionsPlan.GeneratePlan()
		if interpretationErr != nil {
			return startosis_constants.NoOutputObject, nil, interpretationErr.ToAPIType()
		}

		logrus.Debugf("Interpreting the package again with the plan mask succeeded and generated %d new instructions. Adding them to the new optimized plan", attemptInstructionsPlan.Size())
		for _, scheduledInstruction := range attemptInstructionsPlanSequence {
			optimizedPlan.AddScheduledInstruction(scheduledInstruction)
		}

		// finally we can return the optimized plan as well as the serialized script output returned by the last
		// interpretation attempt
		return attemptSerializedScriptOutput, optimizedPlan, nil
	}
}

// Interpret interprets the Starlark script and produce different outputs:
//   - A potential interpretation error that the writer of the script should be aware of (syntax error in the Startosis
//     code, inconsistent). Can be nil if the script was successfully interpreted
//   - The list of Kurtosis instructions that was generated based on the interpretation of the script. It can be empty
//     if the interpretation of the script failed
func (interpreter *StartosisInterpreter) Interpret(
	_ context.Context,
	packageId string,
	mainFunctionName string,
	packageReplaceOptions map[string]string,
	relativePathtoMainFile string,
	serializedStarlark string,
	serializedJsonParams string,
	nonBlockingMode bool,
	enclaveComponents *enclave_structure.EnclaveComponents,
	instructionsPlanMask *resolver.InstructionsPlanMask,
	imageDownloadMode image_download_mode.ImageDownloadMode,
) (string, *instructions_plan.InstructionsPlan, *kurtosis_core_rpc_api_bindings.StarlarkInterpretationError) {
	interpreter.mutex.Lock()
	defer interpreter.mutex.Unlock()
	newInstructionsPlan := instructions_plan.NewInstructionsPlan()
	logrus.Debugf("Interpreting package '%v' with contents '%v' and params '%v'", packageId, serializedStarlark, serializedJsonParams)
	moduleLocator := packageId
	if packageId != startosis_constants.PackageIdPlaceholderForStandaloneScript {
		moduleLocator = path.Join(moduleLocator, relativePathtoMainFile)
	}

	// we use a new cache for every interpretation b/c the content of the module might have changed
	moduleGlobalCache := map[string]*startosis_packages.ModuleCacheEntry{}
	globalVariables, interpretationErr := interpreter.interpretInternal(packageId, moduleLocator, serializedStarlark, newInstructionsPlan, moduleGlobalCache, packageReplaceOptions)
	if interpretationErr != nil {
		return startosis_constants.NoOutputObject, nil, interpretationErr.ToAPIType()
	}

	logrus.Debugf("Successfully interpreted Starlark code into %d instructions", newInstructionsPlan.Size())

	var isUsingDefaultMainFunction bool
	// if the user sends "" or "run" we isUsingDefaultMainFunction to true
	if mainFunctionName == "" || mainFunctionName == runFunctionName {
		mainFunctionName = runFunctionName
		isUsingDefaultMainFunction = true
	}

	if !globalVariables.Has(mainFunctionName) {
		return "", nil, missingMainFunctionError(packageId, mainFunctionName)
	}

	mainFunction, ok := globalVariables[mainFunctionName].(*starlark.Function)
	// if there is an element with the `mainFunctionName` but it isn't a function we have to error as well
	if !ok {
		return startosis_constants.NoOutputObject, nil, missingMainFunctionError(packageId, mainFunctionName)
	}

	runFunctionExecutionThread := newStarlarkThread(moduleLocator)

	var argsTuple starlark.Tuple
	var kwArgs []starlark.Tuple

	mainFuncParamsNum := mainFunction.NumParams()

	// The plan object will always be injected if the first argument name is 'plan'
	// If we are on main, 'plan' must be the first argument
	if mainFuncParamsNum >= minimumParamsRequiredForPlan {
		firstParamName, _ := mainFunction.Param(planParamIndex)
		if firstParamName == planParamName {
			kurtosisPlanInstructions := KurtosisPlanInstructions(packageId, interpreter.serviceNetwork, interpreter.recipeExecutor, interpreter.packageContentProvider, packageReplaceOptions, nonBlockingMode, interpreter.interpretationTimeValueStore, imageDownloadMode)
			planModule := plan_module.PlanModule(newInstructionsPlan, enclaveComponents, interpreter.starlarkValueSerde, instructionsPlanMask, kurtosisPlanInstructions)
			argsTuple = append(argsTuple, planModule)
		}

		if firstParamName != planParamName && isUsingDefaultMainFunction {
			return startosis_constants.NoOutputObject, nil, startosis_errors.NewInterpretationError(unexpectedArgNameError, planParamIndex, planParamName, firstParamName).ToAPIType()
		}
	}

	inputArgs, interpretationError := interpreter.parseInputArgs(runFunctionExecutionThread, serializedJsonParams)
	if interpretationError != nil {
		return startosis_constants.NoOutputObject, nil, interpretationError.ToAPIType()
	}

	// For backwards compatibility, deal with case run(plan, args), where args is a generic dictionary
	runWithGenericDictArgs := false
	if isUsingDefaultMainFunction && mainFuncParamsNum == paramsRequiredForArgs {
		if paramName, _ := mainFunction.Param(argsParamIndex); paramName == argsParamName {
			logrus.Warnf("Using args dictionary as parameter is deprecated. Consider unpacking the dictionary into individual parameters. For example: run(plan, args) to run(plan, param1, param2, ...)")
			argsTuple = append(argsTuple, inputArgs)
			kwArgs = noKwargs
			runWithGenericDictArgs = true
		}
	}
	if !runWithGenericDictArgs {
		argsDict, ok := inputArgs.(*starlark.Dict)
		if !ok {
			return startosis_constants.NoOutputObject, nil, startosis_errors.NewInterpretationError("An error occurred casting input args '%s' to Starlark Dict", inputArgs).ToAPIType()
		}
		kwArgs = append(kwArgs, argsDict.Items()...)
	}

	outputObject, err := starlark.Call(runFunctionExecutionThread, mainFunction, argsTuple, kwArgs)
	if err != nil {
		return startosis_constants.NoOutputObject, nil, generateInterpretationError(err).ToAPIType()
	}

	// Serialize and return the output object. It might contain magic strings that should be resolved post-execution
	if outputObject != starlark.None {
		logrus.Debugf("Starlark output object was: '%s'", outputObject)
		serializedOutputObject, interpretationError := package_io.SerializeOutputObject(runFunctionExecutionThread, outputObject)
		if interpretationError != nil {
			return startosis_constants.NoOutputObject, nil, interpretationError.ToAPIType()
		}
		return serializedOutputObject, newInstructionsPlan, nil
	}
	return startosis_constants.NoOutputObject, newInstructionsPlan, nil
}

func (interpreter *StartosisInterpreter) interpretInternal(
	packageId string,
	moduleLocator string,
	serializedStarlark string,
	instructionPlan *instructions_plan.InstructionsPlan,
	moduleGlobalCache map[string]*startosis_packages.ModuleCacheEntry,
	packageReplaceOptions map[string]string,
) (starlark.StringDict, *startosis_errors.InterpretationError) {
	// We spin up a new thread for every call to interpreterInternal such that the stacktrace provided by the Starlark
	// Go interpreter is relative to each individual thread, and we don't keep accumulating stacktrace entries from the
	// previous calls inside the same thread
	thread := newStarlarkThread(moduleLocator)
	predeclared, interpretationErr := interpreter.buildBindings(packageId, thread, instructionPlan, moduleGlobalCache, packageReplaceOptions)
	if interpretationErr != nil {
		return nil, interpretationErr
	}

	globalVariables, err := starlark.ExecFile(thread, moduleLocator, serializedStarlark, *predeclared)
	if err != nil {
		return nil, generateInterpretationError(err)
	}

	return globalVariables, nil
}

func (interpreter *StartosisInterpreter) buildBindings(
	packageId string,
	thread *starlark.Thread,
	instructionPlan *instructions_plan.InstructionsPlan,
	moduleGlobalCache map[string]*startosis_packages.ModuleCacheEntry,
	packageReplaceOptions map[string]string,
) (*starlark.StringDict, *startosis_errors.InterpretationError) {
	packagePrefix := getModulePrefix(packageId)
	recursiveInterpretForModuleLoading := func(moduleId string, serializedStartosis string) (starlark.StringDict, *startosis_errors.InterpretationError) {
		modulePrefix := getModulePrefix(moduleId)
		if modulePrefix != packagePrefix {
			instructionPlan.AddPackageDependency(modulePrefix)
		}
		result, err := interpreter.interpretInternal(packageId, moduleId, serializedStartosis, instructionPlan, moduleGlobalCache, packageReplaceOptions)
		if err != nil {
			return nil, err
		}
		return result, nil
	}

	kurtosisModule, interpretationErr := builtins.KurtosisModule(thread, interpreter.serviceNetwork.GetEnclaveUuid(), interpreter.enclaveEnvVars)
	if interpretationErr != nil {
		return nil, interpretationErr
	}

	predeclared := Predeclared()
	// Add custom Kurtosis module
	predeclared[builtins.KurtosisModuleName] = kurtosisModule

	// Add all Kurtosis helpers
	for _, kurtosisHelper := range KurtosisHelpers(packageId, recursiveInterpretForModuleLoading, interpreter.packageContentProvider, moduleGlobalCache, packageReplaceOptions) {
		predeclared[kurtosisHelper.Name()] = kurtosisHelper
	}

	// Add all Kurtosis types
	for _, kurtosisTypeConstructors := range KurtosisTypeConstructors() {
		predeclared[kurtosisTypeConstructors.Name()] = kurtosisTypeConstructors
	}

	// Allow the consumers to adjust the builtins
	//
	// This is useful for adding e.g. starlarktest package
	// for unit testing of kurtosis scripts
	if interpreter.processBuiltins != nil {
		predeclared = interpreter.processBuiltins(thread, predeclared)
	}

	return &predeclared, nil
}

const (
	numModIdSeparators = 3
)

// gets the prefix of a module id
// eg. "github.com/kurtosis-tech/postgres-package/main.star" returns "github.com/kurtosis-tech/postgres-package"
func getModulePrefix(moduleId string) string {
	return strings.Join(strings.SplitN(moduleId, git_package_content_provider.OsPathSeparatorString, numModIdSeparators+1)[:numModIdSeparators], git_package_content_provider.OsPathSeparatorString)
}

func findFirstEqualInstructionPastIndex(currentEnclaveInstructionsList []*enclave_plan_persistence.EnclavePlanInstruction, naiveInstructionsList []*instructions_plan.ScheduledInstruction, minIndex int) int {
	if len(naiveInstructionsList) == 0 {
		return -1 // no result as the naiveInstructionsList is empty
	}
	for i := minIndex; i < len(currentEnclaveInstructionsList); i++ {
		// We just need to compare instructions to see if they match, without needing any enclave specific context here
		fakeEnclaveComponent := enclave_structure.NewEnclaveComponents()
		instructionResolutionResult := naiveInstructionsList[0].GetInstruction().TryResolveWith(currentEnclaveInstructionsList[i], fakeEnclaveComponent)
		if instructionResolutionResult == enclave_structure.InstructionIsEqual || instructionResolutionResult == enclave_structure.InstructionIsUpdate {
			return i
		}
	}
	return -1 // no match
}

// This method handles the different cases a Startosis module can be executed.
// - If input args are empty it uses empty JSON ({}) as the input args
// - If input args aren't empty it tries to deserialize them
func (interpreter *StartosisInterpreter) parseInputArgs(thread *starlark.Thread, serializedJsonArgs string) (starlark.Value, *startosis_errors.InterpretationError) {
	// it is a module, and it has input args -> deserialize the JSON input and add it as a struct to the predeclared
	deserializedArgs, interpretationError := package_io.DeserializeArgs(thread, serializedJsonArgs)
	if interpretationError != nil {
		return nil, interpretationError
	}
	return deserializedArgs, nil
}

func makeLoadFunction() func(_ *starlark.Thread, packageId string) (starlark.StringDict, error) {
	return func(_ *starlark.Thread, _ string) (starlark.StringDict, error) {
		return nil, startosis_errors.NewInterpretationError("'load(\"path/to/file.star\", var_in_file=\"var_in_file\")' statement is not available in Kurtosis. Please use instead `module = import(\"path/to/file.star\")` and then `module.var_in_file`")
	}
}

func makePrintFunction() func(*starlark.Thread, string) {
	return func(_ *starlark.Thread, msg string) {
		// the `print` function must be overriden with the custom print builtin in the predeclared map
		// which just exists to throw a nice interpretation error as this itself can't
		panic(print_builtin.UsePlanFromKurtosisInstructionError)
	}
}

func generateInterpretationError(err error) *startosis_errors.InterpretationError {
	switch slError := err.(type) {
	case resolve.Error:
		stacktrace := []startosis_errors.CallFrame{
			*startosis_errors.NewCallFrame(slError.Msg, startosis_errors.NewScriptPosition(slError.Pos.Filename(), slError.Pos.Line, slError.Pos.Col)),
		}
		return startosis_errors.NewInterpretationErrorFromStacktrace(stacktrace)
	case syntax.Error:
		stacktrace := []startosis_errors.CallFrame{
			*startosis_errors.NewCallFrame(slError.Msg, startosis_errors.NewScriptPosition(slError.Pos.Filename(), slError.Pos.Line, slError.Pos.Col)),
		}
		return startosis_errors.NewInterpretationErrorFromStacktrace(stacktrace)
	case resolve.ErrorList:
		// TODO(gb): a bit hacky but it's an acceptable way to wrap multiple errors into a single Interpretation
		//  it's probably not worth adding another level of complexity here to handle InterpretationErrorList
		stacktrace := make([]startosis_errors.CallFrame, 0)
		for _, slErr := range slError {
			if slErr.Msg == skipImportInstructionInStacktraceValue {
				continue
			}
			stacktrace = append(stacktrace, *startosis_errors.NewCallFrame(slErr.Msg, startosis_errors.NewScriptPosition(slErr.Pos.Filename(), slErr.Pos.Line, slErr.Pos.Col)))
		}
		return startosis_errors.NewInterpretationErrorWithCustomMsg(stacktrace, multipleInterpretationErrorMsg)
	case *starlark.EvalError:
		stacktrace := make([]startosis_errors.CallFrame, 0)
		for _, callStack := range slError.CallStack {
			if callStack.Name == skipImportInstructionInStacktraceValue {
				continue
			}
			stacktrace = append(stacktrace, *startosis_errors.NewCallFrame(callStack.Name, startosis_errors.NewScriptPosition(callStack.Pos.Filename(), callStack.Pos.Line, callStack.Pos.Col)))
		}
		var errorMsg string
		// no need to add the evaluation error prefix if the wrapped error already has it
		if strings.HasPrefix(slError.Unwrap().Error(), evaluationErrorPrefix) {
			errorMsg = slError.Unwrap().Error()
		} else {
			errorMsg = fmt.Sprintf("%s%s", evaluationErrorPrefix, slError.Unwrap().Error())
		}
		return startosis_errors.NewInterpretationErrorWithCustomMsg(stacktrace, errorMsg)
	case *startosis_errors.InterpretationError:
		// If it's already an interpretation error -> nothing to convert
		return slError
	}
	return startosis_errors.NewInterpretationError("UnknownError: %s\n", err.Error())
}

func missingMainFunctionError(packageId string, mainFunctionName string) *kurtosis_core_rpc_api_bindings.StarlarkInterpretationError {
	if packageId == startosis_constants.PackageIdPlaceholderForStandaloneScript {
		return startosis_errors.NewInterpretationError(
			"No '%s' function found in the script; a '%s' entrypoint function with the signature `%s(plan, args)` or `%s()` is required in the Kurtosis script",
			mainFunctionName,
			mainFunctionName,
			mainFunctionName,
			mainFunctionName,
		).ToAPIType()
	}

	return startosis_errors.NewInterpretationError(
		"No '%s' function found in the main file of package '%s'; a '%s' entrypoint function with the signature `%s(plan, args)` or `%s()` is required in the main file of the Kurtosis package",
		mainFunctionName,
		packageId,
		mainFunctionName,
		mainFunctionName,
		mainFunctionName,
	).ToAPIType()
}

func newStarlarkThread(threadName string) *starlark.Thread {
	return &starlark.Thread{
		Name:       threadName,
		Print:      makePrintFunction(),
		Load:       makeLoadFunction(),
		OnMaxSteps: nil,
		Steps:      0,
	}
}
