# Native Task CLI Implementation

## Problem Statement
Native tasks (e.g., IBM Cloudability integration) currently lack CLI experience in Terraform. While run tasks have full CLI support with progress tracking and result display, native tasks execute silently without user feedback.

## Solution Overview

The implementation adds CLI support for native tasks by creating a new summarizer that follows the existing pattern used by run tasks and policy evaluations. This provides users with real-time progress updates and detailed outcome displays for native task executions.

## How Run Task CLI Summary Works

### Architecture Overview

The Terraform Cloud backend uses a **summarizer pattern** to display task execution results in the CLI. Each task type (run tasks, native tasks, policy evaluations) has its own summarizer that implements the [`taskStageSummarizer`](terraform/internal/cloud/backend_taskStages.go:32) interface:

```go
type taskStageSummarizer interface {
    Summarize(*IntegrationContext, IntegrationOutputWriter, *tfe.TaskStage) (bool, *string, error)
}
```

### Task Stage Execution Flow

1. **Task Stage Initialization** ([`runTaskStage()`](terraform/internal/cloud/backend_taskStages.go:173))
   - Fetches the task stage with all options including task results
   - Creates summarizers for each task type present:
     - [`newTaskResultSummarizer()`](terraform/internal/cloud/backend_taskStage_taskResults.go:27) - for run tasks
     - [`newNativeTaskResultSummarizer()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:101) - for native tasks
     - `newPolicyEvaluationSummarizer()` - for policy evaluations

2. **Polling Loop** ([`runTaskStage()`](terraform/internal/cloud/backend_taskStages.go:196))
   - Continuously polls the task stage status
   - For native tasks, fetches outcomes via [`fetchTaskResultOutcomes()`](terraform/internal/cloud/backend_taskStages.go:65)
   - Calls each summarizer's `Summarize()` method to display progress

3. **Task Filtering**
   - **Run Tasks**: Filtered by [`filterRunTaskResults()`](terraform/internal/cloud/backend_taskStage_taskResults.go:38) - excludes tasks where `TaskCategory == "native"`
   - **Native Tasks**: Filtered by [`filterNativeTaskResults()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:113) - includes only tasks where `TaskCategory == "native"`

4. **Progress Display**
   - While tasks are pending: Shows count of pending/passed/failed tasks
   - When complete: Displays detailed results with outcomes

### Key Differences: Run Tasks vs Native Tasks

| Aspect | Run Tasks | Native Tasks |
|--------|-----------|--------------|
| **Filtering** | Excludes `TaskCategory == "native"` | Includes only `TaskCategory == "native"` |
| **Outcomes** | Basic status, message, URL | Detailed outcomes (cost, policies, recommendations) |
| **API Fetching** | Standard task result fields | Requires separate outcomes endpoint call |
| **Display Format** | Simple status with enforcement level | Rich formatted output with insights section |
| **Summarizer** | [`taskResultSummarizer`](terraform/internal/cloud/backend_taskStage_taskResults.go:21) | [`nativeTaskResultSummarizer`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:95) |

## CLI-to-Atlas API Interaction

### Overview

The Terraform CLI communicates with **Atlas** (Terraform Enterprise/Cloud backend) through the **go-tfe** client library to fetch task execution results. This interaction follows a polling pattern to retrieve real-time updates on task execution status.

### API Communication Flow

```
┌─────────────────┐         ┌──────────────────┐         ┌─────────────────┐
│  Terraform CLI  │────────▶│   go-tfe Client  │────────▶│  Atlas (TFE)    │
│                 │         │                  │         │                 │
│  - Runs plan    │         │  - HTTP Client   │         │  - Task Stage   │
│  - Polls status │         │  - Auth (Bearer) │         │  - Task Results │
│  - Displays UI  │         │  - JSON API      │         │  - Outcomes     │
└─────────────────┘         └──────────────────┘         └─────────────────┘
```

### Step-by-Step Interaction

#### 1. **Initial Task Stage Fetch**

When a run reaches the task stage (e.g., post-plan), the CLI fetches the task stage information:

**Function**: [`runTaskStages()`](terraform/internal/cloud/backend_taskStages.go:42)
```go
// Fetch run with task stages included
result, err := client.Runs.ReadWithOptions(ctx, runId, &tfe.RunReadOptions{
    Include: []tfe.RunIncludeOpt{tfe.RunTaskStages},
})
```

**API Call**: `GET /api/v2/runs/{run-id}?include=task_stages`

**Response**: Returns run object with embedded task stages

#### 2. **Task Stage Details Fetch**

For each task stage, fetch detailed information including task results:

**Function**: [`getTaskStageWithAllOptions()`](terraform/internal/cloud/backend_taskStages.go:151)
```go
options := tfe.TaskStageReadOptions{
    Include: []tfe.TaskStageIncludeOpt{
        tfe.TaskStageTaskResults,
        tfe.PolicyEvaluationsTaskResults,
    },
}
stage, err := b.client.TaskStages.Read(ctx, stageID, &options)
```

**API Call**: `GET /api/v2/task-stages/{stage-id}?include=task_results,policy_evaluations.task_results`

**Response**: Returns task stage with:
- Task results (status, message, URL, enforcement level)
- Task category (`"native"` or other)
- Policy evaluations

#### 3. **Native Task Outcomes Fetch**

For native tasks, outcomes must be fetched separately (API limitation):

**Function**: [`fetchTaskResultOutcomes()`](terraform/internal/cloud/backend_taskStages.go:65)
```go
// Build URL
url := fmt.Sprintf("%s/task-results/%s/outcomes", baseURL, taskResultID)

// Create authenticated request
req.Header.Set("Authorization", "Bearer "+b.Token)
req.Header.Set("Content-Type", "application/vnd.api+json")
```

**API Call**: `GET /api/v2/task-results/{task-result-id}/outcomes`

**Authentication**: Bearer token from CLI configuration

**Response**: JSON array of outcome objects:
```json
{
  "data": [
    {
      "id": "outcome-123",
      "type": "task-result-outcomes",
      "attributes": {
        "outcome-id": "cost_estimates",
        "url": "https://app.terraform.io/api/v2/task-result-outcomes/outcome-123/body"
      }
    }
  ]
}
```

#### 4. **Outcome Body Fetch**

Each outcome has a body URL that contains the detailed data:

**Function**: [`fetchOutcomeBody()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:276)
```go
// Fetch from outcome URL
req, err := http.NewRequestWithContext(context.Background(), "GET", outcome.URL, nil)
req.Header.Set("Authorization", "Bearer "+ntrs.cloud.Token)
```

**API Call**: `GET /api/v2/task-result-outcomes/{outcome-id}/body`

**Authentication**: Bearer token required

**Response**: JSON body with outcome-specific data:
- **Cost Estimates**: Cost before/after, guardrails, resource diffs
- **Policy Evaluations**: Failed policies, enforcement levels
- **Recommendations**: Resource optimization suggestions

#### 5. **Polling Loop**

The CLI continuously polls for updates until tasks complete:

**Function**: [`runTaskStage()`](terraform/internal/cloud/backend_taskStages.go:196)
```go
return ctx.Poll(taskStageBackoffMin, taskStageBackoffMax, func(i int) (bool, error) {
    // Fetch latest task stage status
    stage, err := b.client.TaskStages.Read(ctx.StopContext, stageID, &options)
    
    // Fetch outcomes for native tasks
    for i, taskResult := range stage.TaskResults {
        if taskResult.TaskCategory == "native" {
            outcomes, err := b.fetchTaskResultOutcomes(ctx, taskResult.ID)
            stage.TaskResults[i].TaskResultOutcomes = outcomes
        }
    }
    
    // Process and display results
    processSummarizers(ctx, output, stage, summarizers, errs)
})
```

**Polling Behavior**:
- **Backoff**: 4-12 seconds between polls
- **Continues while**: Task status is `pending` or `running`
- **Stops when**: Task status is `passed`, `failed`, `errored`, or `canceled`

### Authentication & Authorization

**Token Source**: CLI uses token from:
1. Environment variable: `TF_TOKEN_app_terraform_io`
2. Credentials file: `~/.terraform.d/credentials.tfrc.json`
3. Interactive login: `terraform login`

**Token Usage**:
- Added to all API requests as `Authorization: Bearer <token>`
- Required for:
  - Task stage reads
  - Task result reads
  - Outcome fetches
  - Outcome body fetches

### Error Handling

**Graceful Degradation**:
- If outcomes fetch fails → Display basic task status only
- If outcome body fetch fails → Skip detailed insights
- If task stage unreachable → Display "Skipping task results"

**Example** ([`fetchOutcomeBody()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:276)):
```go
if resp.StatusCode != http.StatusOK {
    return nil  // Gracefully return nil instead of error
}
```

### API Endpoints Summary

| Endpoint | Purpose | Authentication | Used By |
|----------|---------|----------------|---------|
| `GET /api/v2/runs/{id}` | Fetch run with task stages | Bearer token | [`runTaskStages()`](terraform/internal/cloud/backend_taskStages.go:42) |
| `GET /api/v2/task-stages/{id}` | Fetch task stage details | Bearer token | [`getTaskStageWithAllOptions()`](terraform/internal/cloud/backend_taskStages.go:151) |
| `GET /api/v2/task-results/{id}/outcomes` | Fetch native task outcomes | Bearer token | [`fetchTaskResultOutcomes()`](terraform/internal/cloud/backend_taskStages.go:65) |
| `GET /api/v2/task-result-outcomes/{id}/body` | Fetch outcome body data | Bearer token | [`fetchOutcomeBody()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:276) |

### Why Separate Outcomes Endpoint?

The TFE API **does not support** the `include=task_results.task_result_outcomes` parameter. This is why outcomes must be fetched separately:

1. **API Limitation**: The `include` parameter doesn't work for nested outcomes
2. **Workaround**: Manual HTTP request to `/outcomes` endpoint
3. **Manual Parsing**: Direct JSON parsing (jsonapi library incompatible with array responses)
4. **URL Construction**: Convert relative body links to absolute URLs

This is documented in the code at [`fetchTaskResultOutcomes()`](terraform/internal/cloud/backend_taskStages.go:65) lines 65-149.

## Implementation

### Files Created

#### `terraform/internal/cloud/backend_taskStage_nativeTaskResults.go`

**Core Structure:**
```go
type nativeTaskResultSummarizer struct {
    finished bool
    cloud    *Cloud
    counter  int
}
```

**Key Functions:**
- `newNativeTaskResultSummarizer()` - Creates summarizer for native tasks
- `Summarize()` - Main entry point, handles progress and final display
- `filterNativeTaskResults()` - Filters tasks where `TaskCategory == "native"`
- `summarizeNativeTaskResults()` - Counts pending/passed/failed tasks
- `nativeTasksWithTaskResults()` - Displays final results
- `displayDetailedOutcomes()` - Shows integration-specific insights
- `displayCostEstimation()` - Formats cost data with governance
- `displayGovernanceSummary()` - Formats guardrail results
- `displayResourceRecommendationsFromCost()` - Formats optimization suggestions
- `displayPolicyEvaluation()` - Formats policy results
- `fetchOutcomeBody()` - Fetches outcome data with authentication

### Files Modified

#### [`terraform/internal/cloud/backend_taskStages.go`](terraform/internal/cloud/backend_taskStages.go)

**Key Changes:**

1. **Added [`fetchTaskResultOutcomes()`](terraform/internal/cloud/backend_taskStages.go:65) function (lines 65-149)**
   - Fetches TaskResultOutcomes from `/api/v2/task-results/{id}/outcomes` endpoint
   - Required because the API doesn't support the `include` parameter for outcomes
   - Uses manual HTTP request with Bearer token authentication
   - Direct JSON parsing (jsonapi library incompatible with array responses)
   - Constructs absolute URLs from relative body links

2. **Modified [`getTaskStageWithAllOptions()`](terraform/internal/cloud/backend_taskStages.go:151) (lines 160-168)**
   - Fetches outcomes for native tasks after retrieving task stage
   - Attaches outcomes to TaskResult objects

3. **Modified [`runTaskStage()`](terraform/internal/cloud/backend_taskStages.go:173) (lines 187-190)**
   - Added native task summarizer initialization:
   ```go
   // Add native task summarizer
   if s := newNativeTaskResultSummarizer(b, ts); s != nil {
       summarizers = append(summarizers, s)
   }
   ```
   - Fetches outcomes during polling loop (lines 205-213)

#### [`terraform/internal/cloud/backend_taskStage_taskResults.go`](terraform/internal/cloud/backend_taskStage_taskResults.go)

**Key Changes:**

1. **Modified [`filterRunTaskResults()`](terraform/internal/cloud/backend_taskStage_taskResults.go:38) (lines 38-47)**
   - Now explicitly excludes native tasks:
   ```go
   // Exclude native tasks (TaskCategory == "native")
   if task.TaskCategory != "native" {
       runTasks = append(runTasks, task)
   }
   ```
   - This ensures run tasks and native tasks are handled by separate summarizers

2. **Unchanged Core Logic**
   - [`summarizeTaskResults()`](terraform/internal/cloud/backend_taskStage_taskResults.go:78) - counts task statuses
   - [`runTasksWithTaskResults()`](terraform/internal/cloud/backend_taskStage_taskResults.go:107) - displays run task results

### Go-TFE Library Changes

#### `go-tfe/task_result.go`

**Added field to TaskResult struct (line 76):**
```go
// Task result outcomes contain detailed results for native tasks
TaskResultOutcomes []*TaskResultOutcome `jsonapi:"relation,task-result-outcomes,omitempty"`
```

#### `go-tfe/task_stages.go`

**Added include option constant (line 116):**
```go
// Include task result outcomes for native tasks
const TaskResultOutcomesIncludeOpt TaskStageIncludeOpt = "task_results.task_result_outcomes"
```

**Note:** This include option is defined but not supported by the TFE API. Outcomes must be fetched separately via the outcomes endpoint.

#### `go-tfe/run_tasks_integration.go`

**TaskResultOutcome struct (lines 40-47):**

This struct was already defined for run tasks and is reused for native task outcomes:
```go
type TaskResultOutcome struct {
    Type        string                      `jsonapi:"primary,task-result-outcomes"`
    OutcomeID   string                      `jsonapi:"attr,outcome-id,omitempty"`
    Description string                      `jsonapi:"attr,description,omitempty"`
    Body        string                      `jsonapi:"attr,body,omitempty"`
    URL         string                      `jsonapi:"attr,url,omitempty"`
    Tags        map[string][]*TaskResultTag `jsonapi:"attr,tags,omitempty"`
}
```

For native tasks, the key fields used are:
- `OutcomeID` - Identifies the outcome type (`cost_estimates`, `policy_evals`, `recommendations`)
- `URL` - Points to the detailed outcome body data
- `Body` - Contains the outcome data (when fetched from URL)

## API Integration

### TaskResultOutcome Structure
```go
type TaskResultOutcome struct {
    ID         string `jsonapi:"primary,task-result-outcomes"`
    OutcomeID  string `jsonapi:"attr,outcome-id"`
    URL        string `jsonapi:"attr,url"`
    Body       string `jsonapi:"attr,body"`
}
```

### Outcome Types

**1. Cost Estimation (`cost_estimates`):**
```json
{
  "meta": {
    "total_cost_before": "68.97",
    "total_cost_after": "98.97",
    "total_cost_diff": "30.00"
  },
  "result": {
    "cloud_resource_diffs": [...],
    "cost_guardrails": [...]
  }
}
```

**2. Policy Evaluation (`policy_evals`):**
```json
{
  "meta": {
    "passed": false,
    "total_failed_policies": 1
  },
  "result": {
    "failed_policies": [...]
  }
}
```

**3. Recommendations (`recommendations`):**
```json
{
  "result": {
    "recommendations": [...]
  }
}
```

## CLI Output

### During Execution
```
Post-plan Tasks:

1 native tasks still pending, 0 passed, 0 failed ... (4s elapsed)
1 native tasks still pending, 0 passed, 0 failed ... (9s elapsed)
```

### On Completion (PASSED)
```
Running complete!                                    (5s elapsed)

⟶   Overall result: ✓ PASSED
Monthly cost will increase to: $98.97/month
Total monthly cost diff: +$98.97/mo
The IBM Cloudability task was completed successfully
and cost estimation results are ready for viewing.
Resources estimated: 7/20

----------------------------------------------------------------------------
INSIGHTS

Governance Summary: ✓ PASSED

Resource Recommendations: 1
  | Change configuration of staging_test_db from t3.xlarge to t3.medium.
  | Estimated monthly savings: -$95

Policies Summary: ✖ 1/2 policies failed

----------------------------------------------------------------------------
```

### On Completion (FAILED)
```
Running complete!                                    (29s elapsed)

⟶   Overall result: × FAILED
Monthly cost will increase to: $108.62/month
Total monthly cost diff: +$30.00/mo
The IBM Cloudability task was completed successfully
and cost estimation results are ready for viewing.
Resources estimated: 8/20

----------------------------------------------------------------------------
INSIGHTS

Governance Summary: × FAILED
  | Estimated cost increase of +$1,200 exceeds limit of $1,000.
  | Biggest cost change was from a new resource being created.

Resource Recommendations: 1
  | Change configuration of AWS EC2 Instance from t3.xlarge to t3.medium.
  | Estimated monthly savings: -$95

Policies Summary: ✖ 2/6 policies failed
  | Guardrails are Advisory and the plan will still apply.

----------------------------------------------------------------------------
```

## Display Formatting

### Symbols
- `⟶` - Arrow for "Overall result"
- `✓` - Checkmark for PASSED
- `×` - Cross for FAILED
- `✖` - Heavy cross for policy failures
- `|` - Pipe for indented sub-items

### Colors
- `[green]` - Passed status
- `[red]` - Failed status, cost increases
- `[dim]` - Secondary information
- `[bold]` - Headers, labels
- `[reset]` - Reset formatting

### Layout
- 76 dashes (`-`) for section separators
- Blank lines between major sections
- Display order: Overall Result → Governance → Recommendations → Policies

## Native Task Implementation Details

### Core Components

#### 1. Native Task Result Summarizer ([`nativeTaskResultSummarizer`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:95))

**Structure (lines 95-99):**
```go
type nativeTaskResultSummarizer struct {
    finished bool    // Tracks if summarization is complete
    cloud    *Cloud  // Cloud backend reference for API calls
    counter  int     // Tracks number of summarization iterations
}
```

**Key Methods:**

- [`newNativeTaskResultSummarizer()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:101) (lines 101-111)
  - Factory function that creates summarizer only if native tasks exist
  - Filters tasks using [`filterNativeTaskResults()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:113)
  - Returns `nil` if no native tasks found

- [`Summarize()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:127) (lines 127-154)
  - Main entry point called during polling
  - Returns `(bool, *string, error)` indicating whether to continue polling
  - Shows progress messages while tasks are pending
  - Calls [`nativeTasksWithTaskResults()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:186) when complete

- [`filterNativeTaskResults()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:113) (lines 113-121)
  - Filters tasks where `TaskCategory == "native"`
  - Returns only native tasks for processing

- [`summarizeNativeTaskResults()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:156) (lines 156-184)
  - Counts pending, passed, and failed tasks
  - Tracks mandatory enforcement level failures
  - Returns [`nativeTaskResultSummary`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:87) struct

#### 2. Display Functions

- [`nativeTasksWithTaskResults()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:186) (lines 186-242)
  - Main display function for completed tasks
  - Shows "Running complete!" header with elapsed time
  - Calls [`displayDetailedOutcomes()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:244) for each task
  - Handles mandatory enforcement level failures

- [`displayDetailedOutcomes()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:244) (lines 244-274)
  - Fetches outcome bodies using [`fetchOutcomeBody()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:276)
  - Displays outcomes in specific order: cost estimation → policy evaluation
  - Parses JSON outcome bodies into structured data

- [`displayCostEstimation()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:314) (lines 314-349)
  - Displays overall result with cost information
  - Shows monthly cost changes and resource counts
  - Calls governance, recommendations, and policy display functions
  - Formats output with proper symbols and colors

- [`displayGovernanceSummary()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:351) (lines 351-387)
  - Shows cost guardrail status (PASSED/FAILED)
  - Displays failure reasons when guardrails are breached
  - Identifies biggest cost changes

- [`displayResourceRecommendationsFromCost()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:425) (lines 425-445)
  - Shows optimization recommendations
  - Displays instance type changes and estimated savings
  - Extracts resource names from selectors

- [`displayPolicyEvaluation()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:389) (lines 389-423)
  - Shows policy evaluation results
  - Displays failed policy counts
  - Indicates advisory vs mandatory enforcement levels

#### 3. Data Fetching

- [`fetchOutcomeBody()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:276) (lines 276-312)
  - Fetches outcome body content from URL
  - Adds Bearer token authentication
  - Parses JSON into [`cloudabilityOutcomeBody`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:19) struct
  - Returns `nil` on any error (graceful degradation)

#### 4. Data Structures

**Cloudability Outcome Structures (lines 19-86):**
- [`cloudabilityOutcomeBody`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:19) - Top-level outcome container
- [`costEstimationMeta`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:24) - Cost metadata
- [`costEstimationResult`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:29) - Cost details with guardrails
- [`costGuardrail`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:37) - Individual guardrail rules
- [`cloudResourceDiff`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:46) - Resource cost changes
- [`policyEvaluationMeta`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:52) - Policy metadata
- [`policyEvaluationResult`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:58) - Failed policies
- [`recommendationMeta`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:67) - Recommendation metadata
- [`recommendationResult`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:71) - Resource recommendations
- [`cloudResourceRecommendation`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:75) - Individual recommendation
- [`resourceDetails`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:82) - Instance type and pricing

### Key Implementation Details

#### 1. Fetching Outcomes
The TFE API doesn't support `include=task_results.task_result_outcomes`, so outcomes must be fetched separately:
- Endpoint: `/api/v2/task-results/{id}/outcomes`
- Implemented in [`fetchTaskResultOutcomes()`](terraform/internal/cloud/backend_taskStages.go:65)
- Requires Bearer token authentication
- Manual JSON parsing required (jsonapi library incompatible with array responses)
- Called in two places:
  - [`getTaskStageWithAllOptions()`](terraform/internal/cloud/backend_taskStages.go:151) - initial fetch
  - [`runTaskStage()`](terraform/internal/cloud/backend_taskStages.go:173) polling loop - continuous updates

#### 2. Outcome Body Authentication
Outcome body URLs require authentication:
- Add `Authorization: Bearer <token>` header
- Implemented in [`fetchOutcomeBody()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:276)
- Uses `http.DefaultClient` for requests
- Gracefully handles errors by returning `nil`

#### 3. Display Order
Outcomes are displayed in a specific order regardless of API response order:
1. Cost Estimation (includes governance and recommendations)
2. Policy Evaluation

This is enforced in [`displayDetailedOutcomes()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:244) by fetching all outcomes first, then displaying them in the desired order.

#### 4. Cost Values
API returns monthly costs directly - no multiplication needed. Values are parsed as strings and converted to `float64` for display formatting.

#### 5. Task Category Filtering
The implementation uses `TaskCategory` field to distinguish between run tasks and native tasks:
- Run tasks: `TaskCategory != "native"` (filtered by [`filterRunTaskResults()`](terraform/internal/cloud/backend_taskStage_taskResults.go:38))
- Native tasks: `TaskCategory == "native"` (filtered by [`filterNativeTaskResults()`](terraform/internal/cloud/backend_taskStage_nativeTaskResults.go:113))

This ensures each task type is handled by its appropriate summarizer without overlap.

## Testing

```bash
cd /path/to/terraform
go build -o terraform
./terraform plan
```

**Expected Behavior:**
- Progress messages during task execution
- Detailed outcome display on completion
- Proper formatting with colors and symbols
- Correct cost calculations
- Appropriate enforcement level handling