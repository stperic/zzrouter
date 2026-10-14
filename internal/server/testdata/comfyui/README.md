# ComfyUI workflow fixture

`flux_schnell_ui.json` excerpts the four loader/latent nodes from the public
[Comfy-Org FLUX schnell full text-to-image template](https://github.com/Comfy-Org/workflow_templates/blob/main/templates/flux_schnell_full_text_to_image.json),
retrieved 2026-10-13. The model metadata, loader properties, widget values,
node/layout fields and workflow version retain the upstream shape and values.
Unrelated generation, prompt and note nodes are omitted; links/groups are reduced.
This is a metadata parser fixture, not an executable generation workflow.

Tests wrap it in the ComfyUI API submission envelope (`prompt`, `client_id`,
`extra_data.extra_pnginfo.workflow`), and derive deliberate malformed/conflicting
variations. HF transfer fixtures are synthetic HTTP responses and never download
the actual upstream models or contact a ComfyUI server.
