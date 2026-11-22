# myReview — Project Overview

## 1. Purpose

Code review is one of the biggest consumers of a tech lead's time and one of the main sources of delay in delivering code changes. Much of that time goes to work that can be automated: reading every change line by line, spotting common defects, checking for security issues, and deciding whether a change is risky at all.

myReview is a personal review dashboard. It watches a configured set of repositories, runs automated AI reviews on every pull request, and tells the reviewer where to focus. Low-risk changes can be approved with confidence and little effort. High-risk changes arrive with a clear summary and a list of specific points to check.

**Goals**

- Cut the time the reviewer spends on each PR, especially low-risk ones.
- Cut the time PRs wait in review.
- Point the reviewer's attention at the riskiest parts of each change.
- Keep the human in control: the tool recommends, and the reviewer decides.

**Non-goals (v1)**

- Replacing human judgment on high-risk changes.
- Approving PRs on GitHub without the reviewer acting.
- A shared or multi-user service. Each person runs their own instance.
- Notifications outside the dashboard.

## 2. Users

| User | Description |
|------|-------------|
| Reviewer (primary) | A tech lead or senior engineer who reviews many PRs across several repositories. Runs the tool locally, with their own GitHub identity and their own agent CLIs. |
| Other team members (future) | Each runs their own instance with their own configuration and dashboard. Nothing is shared between instances. |

## 3. Key Concepts

- **Watched repositories**: the list of repositories the reviewer configures. Every open PR in them is tracked.
- **Agent**: a local AI coding assistant CLI (Codex or Claude) that the tool runs with a given prompt to do a review step.
- **Review workflow**: the sequence of agent runs done for each PR:
  1. **Code review** and **security review**, which run in parallel.
  2. **Risk assessment**, which runs after both have finished and uses their results.
- **Finding**: one issue reported by the code review or the security review. Each finding has a severity, a location in the code, and a description.
- **Recommendation**: the result of the risk assessment. It is either *automated review is sufficient* or *human review required*, and it comes with a rationale.
- **Repository policy**: settings stored in each repository and maintained by its team: agent and prompt overrides, sensitive areas, a severity threshold, and (in the future) an auto-approval policy.

## 4. Business Requirements

### 4.1 Dashboard

- BR-1: Show every open PR from the watched repositories.
- BR-2: New PRs appear automatically, with no manual refresh or import.
- BR-3: Merged and closed PRs are removed from the dashboard.
- BR-4: Draft PRs are shown, but always ranked below ready PRs.
- BR-5: Default ordering:
  1. PRs that need human review, highest risk first, then longest waiting first.
  2. PRs where automated review is sufficient.
  3. PRs whose review is queued or in progress.
  4. Draft PRs.
- BR-6: The reviewer can filter to the PRs where they are a requested reviewer. All PRs stay visible when the filter is off.
- BR-7: Each PR row shows at least the repository, title, author, age, draft status, workflow status (queued, running, failed, done, outdated), risk level and recommendation.

### 4.2 Review Workflow

- BR-8: The workflow starts automatically when a new PR appears. Exception: a PR matching a `skip` rule in `myreview.json` (by label, title or author) is not reviewed and is shown as skipped.
- BR-9: Code review and security review run in parallel. Risk assessment starts only after both have finished successfully.
- BR-10: When a ready (non-draft) PR gets new commits, the workflow re-runs automatically, and the previous results are marked as outdated. For draft PRs, new commits mark the results as outdated, but the re-run waits until the reviewer starts it.
- BR-11: The reviewer can re-run the whole workflow for any PR at any time.
- BR-12: A limit sets how many PRs are processed at the same time. The rest wait in a queue, and the dashboard shows that they are queued.
- BR-13: If a step fails or times out, the PR shows which step failed and why. Risk assessment does not run on partial results. The reviewer can re-run the workflow.

### 4.3 Configuration

- BR-14: The reviewer configures the list of watched repositories.
- BR-15: Each workflow step (code review, security review, risk assessment) has a default agent (Codex CLI or Claude CLI) and a default prompt.
- BR-16: Each repository has a **repository policy**. It is stored in the repository itself and maintained by that repository's team, so every reviewer's instance applies the same policy. The policy can contain:
  - agent and prompt overrides for any step;
  - sensitive areas (BR-17);
  - a severity threshold (BR-17a).
- BR-17: **Sensitive areas** are parts of the codebase where changes always deserve closer attention, such as authentication, payments, data migrations, infrastructure and public APIs. If a repository does not define its own, these global defaults apply: authentication, authorization, and the data model.
- BR-17a: The **severity threshold** is the lowest finding severity that requires human review. Strict repositories can set it to Low, so any finding needs a human. Others can set it to High or Critical. If a repository does not set one, the threshold is High.
- BR-18: The concurrency limit (BR-12) can be configured.

### 4.4 PR Details

When the reviewer opens a PR, the details view shows:

- BR-19: **Overview**: a short, plain-language summary of what the PR changes and why.
- BR-20: **Code review report**: a list of findings, each with severity, file and line, and description.
- BR-21: **Security report**: a list of security findings in the same format.
- BR-22: **Points of interest**: specific places the reviewer should look at personally, each with the reason.
- BR-23: **Risk recommendation**: the risk level, the recommendation (automated review is sufficient or human review required), and the rationale behind it.
- BR-24: **Controls**: re-run the review workflow, and approve the PR.
- BR-25: A link to the PR on GitHub.

### 4.5 Actions on GitHub

- BR-26: **Post finding as comment**: for any finding, the reviewer can post it as an inline comment on the matching file and line of the GitHub PR. The text can be edited before posting. A finding that has already been posted is marked as posted.
- BR-27: **Approve**: submits an approval on GitHub under the reviewer's identity, so the reviewer does not have to open GitHub.
- BR-27a: If the PR is marked *human review required*, Approve first shows a confirmation that lists what triggered the recommendation (the findings and sensitive areas). The approval is submitted only after the reviewer confirms.
- BR-28: In v1, every action on GitHub (comments and approvals) is taken only when the reviewer explicitly asks for it. The tool never posts or approves on its own.

## 5. Risk Assessment Criteria

The risk assessment decides whether a PR can rely on automated review or needs a human. In v1 it weighs two factors:

**1. Severity of findings** (from the code review and the security review)

| Severity | Meaning |
|----------|---------|
| Critical | Likely security vulnerability, data loss or corruption, or an outage. |
| High | A real defect or security weakness that will probably cause incorrect behavior. |
| Medium | A probable issue or significant maintainability concern. |
| Low | Minor, stylistic, or a suggestion. |

**2. Sensitive areas touched** (as defined in the repository)

**Decision rules**

Each repository can set its own severity threshold (BR-17a). The default is High.

- **Human review required** if *any* of these are true:
  - any finding, from either review, is at or above the repository's severity threshold;
  - the change touches a sensitive area of the repository.
- **Automated review is sufficient** otherwise, which means all findings are below the threshold and no sensitive areas are touched.
- The rationale must say which rule or rules triggered the decision. Every finding at or above the threshold, and every sensitive area touched, is also listed as a point of interest.

**Risk level** shown on the dashboard. It describes how serious the change is, regardless of the repository's threshold:

| Level | When |
|-------|------|
| High | A Critical finding, or a sensitive area touched together with a High finding. |
| Medium | Human review required, but the conditions for High are not met. |
| Low | Automated review is sufficient. |

## 6. User Flows

### 6.1 Initial setup

1. The reviewer configures the watched repositories.
2. The reviewer sets the default agent and prompt for each workflow step.
3. Each repository's policy (agent and prompt overrides, sensitive areas, severity threshold) is read from the repository itself. Global defaults apply wherever the repository doesn't define a setting.
4. The reviewer sets the concurrency limit.
5. The dashboard loads all open PRs and queues them for review.

### 6.2 A new PR arrives

1. A new PR is opened in a watched repository.
2. It appears on the dashboard with status *queued*.
3. When a slot is free, code review and security review start in parallel (status *running*).
4. When both finish, the risk assessment runs.
5. The PR shows its risk level and recommendation and moves to its position in the ordering.

### 6.3 Approving a low-risk PR

1. The reviewer sees a PR marked *automated review is sufficient* with low risk.
2. The reviewer opens the details and reads the overview and reports.
3. The reviewer clicks **Approve**. The approval is submitted on GitHub.

### 6.4 Reviewing a high-risk PR

1. The reviewer opens a PR marked *human review required*.
2. The reviewer reads the overview, the rationale, and the points of interest.
3. The reviewer goes through the findings and points, checks them off, and for each relevant one adjusts the suggested comment and adds it to the review.
4. The reviewer reviews the points of interest in detail, on GitHub or in their own tools.
5. The reviewer picks Approve, Request changes or Comment and clicks **Submit review**. The dialog lists the comments to post and anything an approval must confirm; submitting posts one GitHub review with the decision and all comments.

### 6.5 PR updated with new commits

1. The author pushes new commits.
2. The previous results are marked *outdated*.
3. For a ready PR, the workflow re-runs automatically. For a draft PR, the results stay outdated until the reviewer clicks **Re-run**.
4. Comments that were already posted stay on GitHub.

### 6.6 Draft becomes ready

1. The author marks a draft PR as ready for review.
2. The PR moves out of the draft section. If its results are outdated or missing, the workflow runs.

### 6.7 Review step fails

1. An agent run fails or times out.
2. The PR shows *failed*, with the failed step and the reason.
3. The reviewer fixes the cause if needed (for example, agent configuration) and clicks **Re-run**.

### 6.8 PR merged or closed

1. The PR is merged or closed on GitHub.
2. It disappears from the dashboard.

## 7. Success Measures

- Less reviewer time spent per PR, especially on PRs where automated review is sufficient.
- Shorter time from a PR being opened to its first review action.
- Agreement rate: how often the reviewer agrees with the recommendation. This shows whether the criteria and prompts need tuning.
- A low rate of issues found after merge in PRs that were approved on an "automated review is sufficient" recommendation.

## 8. Future Considerations

- Per-repository auto-approval policy: the tool approves on its own when the recommendation is "automated review is sufficient" and the policy allows it.
- More risk factors: change size and scope, test coverage of the change, and the author's familiarity with the codebase.
- Submitting several findings together as one GitHub review.
- Notifications when a PR needs human review.
- A history view of merged and closed PRs, with past recommendations.

## 9. Open Questions

None at the moment.
