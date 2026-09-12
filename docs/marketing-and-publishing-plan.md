# Marketing site and weekly publishing plan

## Product source of truth

The site must describe the forthcoming native Marathon release represented by
the current repository README, not the existing `v0.1.0` companion release.

The public product message is:

> CodexMarathon is a native multi-account workflow inside a custom Codex CLI.
> It switches managed ChatGPT identities only at safe boundaries between turns,
> preserving the active conversation.

Use these verified product facts in all generated content:

* one custom `codex` executable, with `codex marathon ...` commands;
* matching `/marathon ...` commands in interactive Codex sessions;
* named managed account profiles;
* browser-link and device-code login;
* safe turn-boundary switching, target identity verification, and opaque
  credential snapshots;
* fresh quota observations and reset-time revalidation;
* no real provider quota-reset action in the release;
* API keys, external bearer auth, workload identity, and unsupported provider
  modes are not switchable.

Never position the product as a way to bypass limits, use free accounts, or
evade provider policy.

## Architecture

Use an Astro static site under `site/` and deploy it through GitHub Actions.
This requires no database, CMS, backend, or manual publishing interface.

```text
topic queue + local project documentation
             |
             v
weekly GitHub Actions generator
             |
             |-- MDX article + TOC
             |-- JSON-LD
             |-- sitemap.xml, RSS, llms.txt
             |-- build and quality checks
             `-- direct commit to main, then deploy
```

The workflow publishes directly to `main` after all checks pass. This is the
requested automatic approval model and has less maintenance than generating a
pull request plus an auto-merge bot.

## Macro task 1 — site foundation

Create a static Astro site with:

* MDX article collection at `site/src/content/blog/`;
* a single product-context module containing the canonical product facts;
* a shared SEO component for title, canonical URL, Open Graph fields, and
  JSON-LD;
* generated `sitemap.xml`, `robots.txt`, RSS, and build-time `llms.txt`;
* GitHub Pages or a static-host deployment workflow.

`llms.txt` is regenerated on every build from published pages, like the
sitemap. It is useful AI-agent discovery metadata, but is not described as an
indexing mechanism.

## Macro task 2 — landing page

### SEO target

* Primary: `codex account switcher`
* Secondary: `codex multi account`
* Differentiator: `switch Codex accounts without losing your conversation`

### Metadata

```text
Title: CodexMarathon — Safe Codex Account Switcher for the Codex CLI

Description: Native multi-account switching for Codex. Change managed ChatGPT
accounts at safe turn boundaries, preserve conversations, and make
usage-aware account decisions.
```

### Required sections

1. **Hero** — “A safe Codex account switcher that preserves your conversation.”
2. **Native workflow** — show `codex marathon status`, login, enable, and
   switch commands.
3. **Why safe boundaries matter** — explain that no active model response,
   tool call, or subagent turn is changed mid-flight.
4. **Usage-aware account selection** — explain fresh observations, exhausted
   account avoidance, and reset revalidation. Do not call this quota bypass.
5. **Features** — multiple profiles, device-code login, status line,
   target-identity verification, recovery handling, and secret-safe storage.
6. **Install the latest release** — dynamically query the repository's public
   GitHub `releases/latest` data at build time. Render only actual release
   assets and commands appropriate to their platform.
7. **Quick start** — copyable native Marathon commands.
8. **FAQ** — switching, account profiles, safe continuation, and usage-limit
   behavior.
9. **Feedback** — link directly to a prefilled GitHub issue form and, if
   enabled, GitHub Discussions. No custom form backend.

### Reusable landing-page template

Use a single-page, anchor-linked product narrative. The structural inspiration
is the reference page's immediate install path, visual proof, compact feature
storytelling, and repeated call to action; the CodexMarathon implementation
must use its own brand, artwork, copy, and layout.

```text
sticky navigation
  logo | Overview | How it works | Features | Install | Feedback | GitHub

hero: first viewport
  eyebrow: NATIVE MULTI-ACCOUNT WORKFLOW FOR THE CODEX CLI
  H1: Safe Codex account switching, without losing the conversation.
  one-sentence outcome and short proof paragraph
  [copyable latest-release install command] [copy]
  [View GitHub release] [Read the docs]
  product screenshot or animated terminal capture

proof strip
  Native commands · Safe turn boundary · Same-thread resume · Opaque credentials

how it works
  01 wait for a safe boundary
  02 select a fresh eligible managed account
  03 reload or restart and verify identity
  04 resume the same conversation exactly once

feature gallery
  one screen or terminal capture per feature
  profile management | device-code login | status and quota view | recovery

quota-aware selection
  an explanatory diagram, not a claim of unlimited use

install section
  latest GitHub release card | platform asset picker | verify command

FAQ + feedback
  real user questions | issue/discussion links

final CTA
  copyable install command | GitHub release link | source code link
```

### Above-the-fold requirements

The first viewport must let a visitor answer these questions without scrolling:

1. What is this? A native, safe Codex account switcher.
2. Why use it? It preserves the active conversation across a managed account
   transition.
3. How do I start? Copy the current release's platform-specific install
   command or open its GitHub release.
4. Can I trust it? The visible proof points link to source code, release notes,
   and the documented safety model.

### Screens and visual proof

Create original visual assets from the real product; do not use stock terminal
screens or reproduce the reference site's visuals.

Required captures:

* **Native command overview:** `codex marathon status` with enabled state,
  active alias, and managed-account count. Never show credential material.
* **Account login:** browser-link and device-code choices, with the code itself
  redacted or replaced by a realistic non-live fixture.
* **Safe switching:** a compact four-step transition diagram and terminal
  confirmation of the verified target identity.
* **Usage-aware selection:** a simple bucket/decision illustration showing
  fresh observations, exhausted-account avoidance, and reset revalidation.
* **Recovery:** same-thread resume shown as a timeline, not an invented product
  UI.

Each screen must have descriptive alt text. Use an image caption to state what
the visitor is seeing and link to the relevant documentation section.

### Interaction and accessibility rules

* Navigation links scroll to semantic sections and retain visible focus.
* Every install command has a keyboard-accessible copy button plus a visible
  fallback download/release link.
* Respect `prefers-reduced-motion`; screenshots and diagrams must convey the
  complete story without animation.
* Keep the primary install CTA in the hero and repeat it after the feature
  gallery and at the end of the page.
* Do not put essential installation or safety information only in an image,
  hover effect, tab, or video.

### Schema

Generate JSON-LD from visible content:

* `Organization` and `WebSite` site-wide;
* `SoftwareApplication` for the landing page;
* `BreadcrumbList` for non-home pages;
* `FAQPage` only when the visible page genuinely contains those questions and
  answers.

## Macro task 3 — content model

Each article contains validated frontmatter:

```yaml
title: How to Switch Codex Accounts Without Losing Your Conversation
description: A safe, native workflow for changing a managed ChatGPT account in Codex.
publishDate: 2026-09-14
keyword: switch Codex account without losing conversation
intent: informational
status: published
related:
  - /
  - /guides/multiple-codex-accounts/
```

Every article automatically gets:

* a visible H1 and accessible table of contents generated from H2/H3 headings;
* canonical URL, metadata, and Open Graph image;
* `Article` and `BreadcrumbList` JSON-LD;
* contextual links to the landing page and related guides;
* entries in RSS, sitemap, and `llms.txt`.

Use JSON-LD rather than duplicate JSON-LD and Microdata. Both are valid
Schema.org serializations, but a generated JSON-LD block is less fragile for
MDX and is simpler to validate automatically.

## Macro task 4 — editorial queue

Keep a pre-approved `site/src/content/topics.json` queue of at least twelve
topics. Ubersuggest is used only during a short monthly review, not in CI,
because its OAuth MCP session and free-tier daily report limit are unsuitable
for unattended GitHub Actions.

Initial cluster:

1. How to switch Codex accounts without losing your conversation
2. Use multiple Codex accounts safely in the Codex CLI
3. Codex account switcher: native safe-boundary switching explained
4. What happens when a Codex account reaches a usage limit?
5. Use device-code login for Codex accounts on a server
6. Why switching `auth.json` mid-turn is unsafe
7. How CodexMarathon preserves a thread during account changes
8. Understand fresh quota observations and reset revalidation
9. Manage work and personal Codex accounts safely
10. Recover from an interrupted Codex account transition
11. Import and name an existing Codex identity
12. CodexMarathon versus proxy-based account rotation

## Macro task 5 — weekly automatic publication

Create `.github/workflows/publish-weekly-article.yml`.

**Schedule:** Monday at 09:15 UTC, plus `workflow_dispatch` for recovery.

```text
select highest-priority queued topic
  → load current local README, release notes, and documentation facts
  → generate original MDX with a configured model API
  → perform a separate structured quality review
  → validate article data, links, headings, and schema inputs
  → regenerate sitemap, RSS, and llms.txt
  → build the site
  → commit directly to main and deploy
```

### Non-negotiable publish gates

The workflow must stop without committing when:

* no queued topic exists, or the topic was already published;
* frontmatter is invalid, duplicated, or missing a canonical URL;
* the article lacks project-specific technical facts;
* required internal links are missing or broken;
* H2/H3 headings cannot create a valid table of contents;
* generated JSON-LD inputs are invalid;
* the site build, link check, sitemap, RSS, or `llms.txt` generation fails;
* the prose claims unlimited usage, quota evasion, bans, or unsupported
  product behavior.

### Required secrets and permissions

* `OPENAI_API_KEY` as a GitHub Actions secret for generation and review;
* minimal `GITHUB_TOKEN` `contents: write` permission for the publication
  commit;
* workflow concurrency to prevent duplicate weekly posts;
* no Ubersuggest secret or OAuth token in CI.

## Macro task 6 — discovery and minimum maintenance

One-time setup:

1. Set the canonical site domain.
2. Submit the generated sitemap in Google Search Console.
3. Add the site URL to the GitHub repository About section and release notes.

Monthly maintenance, target time: 20 minutes:

1. Use Ubersuggest to inspect unused long-tail terms and live SERPs.
2. Add four or more qualified topics to the queue.
3. Check Search Console impressions and clicks.
4. Promote successful topics into pillar links; remove irrelevant topics.

No other recurring operational work is required.

## Build order

1. Astro site and shared product context.
2. Native-Marathon landing page.
3. JSON-LD, sitemap, RSS, robots, and build-time `llms.txt`.
4. Topic queue and first twelve articles briefs.
5. Local article generator and quality gates.
6. Manual GitHub Actions dry run.
7. Scheduled direct-to-main publishing.
8. Search Console connection and monthly review.
