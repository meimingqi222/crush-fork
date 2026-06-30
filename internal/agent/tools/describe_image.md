Describe an image file using a vision-capable helper model. This tool is available when the primary model does not support image inputs and a vision helper model has been configured via the "vision" model slot in crush.json.

## When to use

Use this tool when you need to understand the content of an image file or pasted image attachment (e.g., a screenshot, diagram, photo, or chart) and the current model cannot process images directly. The tool reads the image, sends it to a separately configured vision model, and returns a text description.

## Parameters

- `path` (optional): Path to an image file. Supports jpg, jpeg, png, gif, and webp formats. Use this for real files on disk.
- `message_id` (optional): Message ID containing a pasted image attachment. Use this with `image_index` when the image placeholder includes them.
- `image_index` (optional): 1-based image attachment index within `message_id`.
- `prompt` (optional): Custom instruction for what to focus on in the description (e.g., "Extract all text visible in this screenshot" or "Describe the UI layout and components").

## Notes

- If the primary model already supports images, this tool is not registered — use the `read` tool instead.
- The vision model is configured separately in crush.json under the `models.vision` key.
- For pasted/chat images, prefer `message_id` and `image_index` over `path`; filenames like `paste_1.png` may repeat across turns.
- Results are cached for 30 minutes to avoid redundant API calls for the same image.
