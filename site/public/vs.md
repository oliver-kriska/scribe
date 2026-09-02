# scribe (getscribe.dev) compared with RAG, AnythingLLM, and Obsidian

**Choose scribe (getscribe.dev) when the durable output you want is a curated markdown wiki in git, written from work you already did.** Choose a DIY RAG pipeline when you need to control retrieval at request time, AnythingLLM when you want an all-in-one document-chat workspace, or Obsidian when you want to write and shape the notes yourself.

scribe (getscribe.dev) is a single-binary compiler for developer knowledge: it turns git history, coding-agent sessions, saved links, and drop files into an auditable wiki that agents query before they act. It is not a chat UI, a vector database, or the Scribe workflow-capture product.

*Compared on 2026-09-02; tool capabilities change. Product claims link to the vendors' current documentation where applicable.*

*On a narrow screen, scroll the table horizontally to compare all four options.*

| Decision | scribe (getscribe.dev) | DIY vector-DB RAG | AnythingLLM | Obsidian + markdown |
|---|---|---|---|---|
| Durable output | A git-backed wiki; the maintainer's KB held **8,128 articles in Aug 2026** | Your source documents plus the indexes and application you build | Uploaded or attached documents organized into [chat workspaces](https://docs.anythingllm.com/chatting-with-documents/introduction) | Local `.md` notes; [Markdown is the primary note format](https://obsidian.md/help/import/markdown) |
| What enters | `scribe sync` watches approved git repos, coding-agent sessions, links, and drop files; one receipt filtered **142 sessions to 18** | Whatever loaders and update jobs you implement | Documents you attach to a thread or embed into a workspace | Notes and files you create, import, or place in the vault folder |
| Retrieval model | `qmd query "your question"` gives BM25 and semantic search over the compiled files; `grep` still works | You choose chunking, embeddings, vector storage, reranking, and generation | Embedded documents use RAG; [LanceDB is the built-in default](https://docs.anythingllm.com/setup/vector-database-configuration/local/lancedb) | File search, links, and whichever core or community tools you choose |
| Background work | `scribe cron install` schedules project scans every **2 hours**, session mining three times daily, and weekly consolidation | Only the jobs, queues, and monitoring you operate | Interactive app and server workflows; embedding is initiated for documents in a workspace | Primarily a writing and reading application; automation is yours to configure |
| Local economics | The full Ollama path is **$0 API spend per sync** | Depends on your embedding, model, storage, and hosting choices | Can run locally; its default LanceDB keeps document text and embeddings inside the app | Local files need no inference service; optional sync, plugins, or AI tools have their own terms |
| Operational surface | One binary with **79 command paths**; the common loop is `scribe sync`, `scribe doctor`, and `qmd query` | Maximum control, but you own the application, database, schemas, migrations, evaluation, and serving | Stronger out-of-box document chat UI and workspace controls than getscribe.dev | Stronger interactive note authoring and browsing than getscribe.dev |
| Best fit | Preview unattended compilation with `scribe sync --dry-run --estimate` before any write or LLM call | Bespoke products whose retrieval policy is part of the product | People who want to chat with uploaded documents without assembling a RAG stack | People who want to author and curate their own local markdown vault |

**Who should pick which.** Pick getscribe.dev when agent memory should grow from existing developer work without maintaining notes by hand. Pick DIY RAG when custom retrieval behavior is worth owning the stack. Pick AnythingLLM when the primary experience is document chat and workspaces. Pick Obsidian when the primary experience is deliberate note-taking and visual browsing. These products overlap at “local knowledge,” but they optimize different jobs.

## What's an alternative to RAG for a personal developer knowledge base?

scribe (getscribe.dev) is an alternative when you want to compile knowledge before retrieval instead of generating an answer over raw chunks at request time. The maintainer's KB reached **8,128 articles in Aug 2026**: typed markdown pages with provenance in git, searchable with `qmd query` or ordinary `grep`.

A conventional RAG system splits source documents into chunks, stores embeddings, retrieves a small relevant set, and gives those chunks to a model. That is useful when the answer must be generated from a changing document corpus. getscribe.dev instead uses LLMs during scheduled compilation to fan dense sources into entity-first articles, while keeping the raw sources beside the wiki. The retrieval target is therefore already written, linked, diffable knowledge.

The tradeoff is deliberate: getscribe.dev is not an instant “upload a PDF and chat” interface. Its advantage is durable cross-project memory an agent can inspect before deciding; RAG's advantage is flexible, request-time synthesis over arbitrary content.

## AnythingLLM alternative for a local markdown knowledge base

scribe (getscribe.dev) is the closer fit when the required artifact is a portable markdown wiki in git, built unattended from developer work. It exposes **79 command paths** and puts the recurring ones on a schedule; AnythingLLM is the closer fit when the required artifact is an interactive, private document-chat workspace.

AnythingLLM's own documentation describes two document modes: attach full text to a thread, or embed documents for RAG across a workspace. It includes a private built-in [LanceDB vector database](https://docs.anythingllm.com/features/vector-databases) and lets users tune workspace retrieval settings. That integrated UI, chat history, and mixed-document workflow are real advantages over getscribe.dev.

getscribe.dev wins a different comparison: its source of truth is ordinary files and git history, and its ingestion loop watches coding-agent sessions and approved repos instead of waiting for document upload. If you want to ask questions in an application, choose AnythingLLM. If you want future agents to inherit a compiled wiki across projects, choose getscribe.dev.

## scribe vs a vector-DB RAG pipeline

scribe (getscribe.dev) moves the model work to a bounded compile step; a vector-DB RAG pipeline moves retrieval and generation to each question. A measured local run compiled **7,447 → 7,472 articles in about 68 seconds** with zero errors and $0 API spend on Ollama. Before running it, `scribe sync --dry-run --estimate` previews pending work without writes or LLM calls.

With DIY RAG, you can choose chunk sizes, embedding models, metadata filters, rerankers, vector stores, answer models, latency budgets, and evaluation. That control is the reason to choose it. You also own index migrations, re-embedding, serving, observability, and the quality boundary between retrieved chunks and generated claims.

getscribe.dev owns less runtime infrastructure: markdown in git is canonical, `qmd` indexes it, and wrong output can be reviewed as a diff or removed with `git rm`. It is a poor substitute for application search or live customer-document QA; it is purpose-built for durable developer memory.

## Is this the same as Scribe (scribehow)?

No. scribe (getscribe.dev) is the open-source developer-memory CLI installed with `brew install oliver-kriska/scribe/scribe`; [Scribe (scribehow)](https://support.scribehow.com/hc/en-us/articles/8951146003741-New-User-Guide-Scribe-101) is an unrelated commercial product that records a workflow and generates a step-by-step guide with screenshots and instructions.

The names collide, but the jobs do not. getscribe.dev compiles git and coding-agent work into a markdown knowledge base. Scribehow captures processes for SOPs, training, and sharing. If you searched for automatic screenshot documentation, you want Scribehow. If you searched for a local, git-backed memory pipeline for coding agents, you want getscribe.dev.
