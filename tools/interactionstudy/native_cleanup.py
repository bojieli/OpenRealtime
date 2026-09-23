"""Attempt every native trial cleanup step without losing earlier failures."""
import asyncio


async def finish(inference, speech, recorder):
    failures = []
    if inference is not None:
        try:
            await asyncio.shield(inference)
        except Exception as exc:
            failures.append("inference: " + repr(exc))
    for name, component in (("speech", speech), ("recorder", recorder)):
        try:
            await component.close()
        except Exception as exc:
            failures.append(name + ": " + repr(exc))
    return failures
