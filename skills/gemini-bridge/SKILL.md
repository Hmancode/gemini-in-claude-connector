---
name: gemini-bridge
description: Use the Gemini bridge tools (ask_gemini) for audio/video, an independent cross-check of facts, math or code, or very large files; covers prompting, model choice, limits and privacy.
---

# Gemini bridge

The Gemini bridge gives you Google's Gemini models through the user's own API key. Its tools are `ask_gemini`, `gemini_generate_image` and `gemini_list_models`. They may appear with a prefix such as `gemini__ask_gemini`.

Gemini sees only what you send it. Every call costs time and uses the user's quota. Use it when it adds something you can't do as well yourself.

## Best uses, in order

1. **Audio and video.** Gemini listens to and watches media directly: lecture recordings, voice memos, interviews, meetings and YouTube talks. Get a transcript or structured notes from Gemini, then do the analysis and writing yourself from that.
2. **An independent cross-check.** Use it before the user relies on something you produced, such as a calculation, a statistics result, a proof, a non-trivial code change or a factual claim. A model trained differently catches different mistakes.
3. **Very large files.** Long PDFs, big text or log files, and multi-hour transcripts belong here. Send the file and ask targeted questions, so the whole thing doesn't fill your context.
4. **The user asks for Gemini or a second opinion by name.** Use ask_gemini with their request.

## Don't use it for

- **Things you already do well.** That costs a round trip and quota for nothing.
- **Web search.** Use your own search tools. Gemini's `search` option is often not included on free keys.
- **Images, unless the key includes them.** Free keys get 0 image requests. Suggest the Gemini app instead (Win+G if the user has the hotkeys).
- **Getting around your own judgment.** If you decline a request, don't hand it to Gemini instead.

## Writing the call

- **`prompt`:** make it self-contained. State the goal, the context Gemini needs, the output format and the length. Gemini can't see this conversation.
- **`files`:** use absolute paths on the user's computer, only for files the user pointed you to. Accepted types are PDF, images, audio, video, text and code, .docx and .pptx. For .xlsx or .doc, ask the user to export a PDF or CSV first.
- **`urls`:** YouTube links are watched; other pages are read.
- **`conversation_id`:** use any short label. Reuse it for follow-ups so Gemini keeps the thread.
- **`model`:** leave it at the default, `flash`, which falls back to `flash-lite` automatically when Flash is busy. Use `flash-lite` for quick, simple asks. Only use `pro` if the user's key includes it; on a free key it falls back to `flash`, so don't promise Pro-level answers.

## Cross-check protocol

1. Work the problem yourself first.
2. Send Gemini the **problem, not your answer**, and ask it to solve it independently and show its steps. Showing your answer first anchors it.
3. Compare the two:
   - If they agree, say so in one line.
   - If they differ, find the exact step where they part ways and check that step. Don't average the answers or pick one by vote.
4. Optionally, send your answer afterward and ask Gemini to look for errors in it.
5. Tell the user what was checked, whether the answers agreed, and where they differed.

## Audio and video protocol

- **What to ask for:**
  - a transcript or outline with `[mm:ss]` timestamps
  - speaker labels if there are several speakers
  - the main claims, decisions and action items
- **Long recordings:** ask for an outline first, then for the sections that matter.
- **Wait time:** big files are uploaded and processed by Google before Gemini can answer, so tell the user it may take a minute.
- **Before quoting:** check specific quotes and numbers against the transcript timestamps before you rely on them.

## Presenting results

- **Say which parts came from Gemini.** Its output is another model's work: check claims that matter before stating them as fact.
- **The `[Answered by …]` tag is authoritative.** It names the model that actually answered. Gemini sometimes misreports its own model name, so trust the tag over what it says about itself.
- **Mention a fallback only if it matters.** For example, if the user asked for Pro and Flash answered, say so.

## Errors

- **"isn't included on this API key"** (Pro, images, search): don't retry. Tell the user in one line that it needs billing on the key or the Gemini app, then carry on with what works.
- **"overloaded right now":** the bridge has already retried and fallen back. Try once more later or use `flash-lite`. Never loop.
- **"rate limit … retry in about X":** tell the user the wait time and continue without Gemini.
- **Refused by Gemini's content rules:** report it plainly. Don't rephrase the request to get it through.

## Privacy

- **Where data goes:** files and prompts go to Google. On free keys, Google may use what's sent to improve its products.
- **Ask first** before sending personal, medical or financial details, or other people's private messages, emails or documents.
- **Credential files:** the bridge refuses keys and credentials files. Don't try to work around that.
