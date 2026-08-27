"""JSON-lines bridge from the native macOS app to browser-use.

This is intentionally an adapter, not a second agent. OpenRealtime remains
the model and action-policy owner. browser-use supplies its DOM serializer,
stable selector map, Python set-of-mark renderer, and hardened click paths.
Each response is one JSON object on stdout; logs are redirected to stderr so
they can never corrupt the control stream.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
import sys
from typing import Any

from browser_use.browser import BrowserProfile, BrowserSession
from browser_use.browser.events import (
	ClickCoordinateEvent,
	ClickElementEvent,
	ScrollEvent,
	SendKeysEvent,
	TypeTextEvent,
)
from browser_use.browser.python_highlights import create_highlighted_screenshot_async


class Bridge:
	"""Own one browser-use session and its most recently visible mark map."""

	def __init__(self, cdp_url: str) -> None:
		self.session = BrowserSession(
			browser_profile=BrowserProfile(cdp_url=cdp_url, is_local=True, keep_alive=True)
		)
		self.selector_map: dict[int, Any] = {}
		self.focused_node: Any | None = None

	async def start(self) -> None:
		await self.session.start()

	async def close(self) -> None:
		await self.session.stop()

	async def dispatch(self, message: dict[str, Any]) -> dict[str, Any]:
		command = message.get("command")
		if command == "capture":
			return await self.capture()
		if command == "screenshot":
			return await self.capture()
		if command == "click_element":
			index = int(message["element_id"])
			node = self.selector_map.get(index)
			if node is None:
				raise ValueError(f"set-of-mark element {index} is absent from the current frame")
			await self.session.event_bus.dispatch(ClickElementEvent(node=node))
			self.focused_node = node
			return {"status": "clicked", "element_id": str(index)}
		if command == "click":
			x, y = int(message["x"]), int(message["y"])
			button = str(message.get("button", "left"))
			await self.session.event_bus.dispatch(
				ClickCoordinateEvent(coordinate_x=x, coordinate_y=y, button=button)
			)
			return {"status": "clicked"}
		if command == "double_click":
			await self.click_mouse(int(message["x"]), int(message["y"]), button="left", count=2)
			return {"status": "clicked"}
		if command == "move":
			await self.mouse_event("mouseMoved", int(message["x"]), int(message["y"]))
			return {"status": "moved"}
		if command == "drag":
			from_x, from_y = int(message["from_x"]), int(message["from_y"])
			to_x, to_y = int(message["to_x"]), int(message["to_y"])
			await self.mouse_event("mousePressed", from_x, from_y, button="left", buttons=1)
			for step in range(1, 6):
				x = round(from_x + (to_x - from_x) * step / 5)
				y = round(from_y + (to_y - from_y) * step / 5)
				await self.mouse_event("mouseMoved", x, y, button="left", buttons=1)
			await self.mouse_event("mouseReleased", to_x, to_y, button="left", buttons=0)
			return {"status": "dragged"}
		if command == "type":
			index = message.get("element_id")
			node = self.selector_map.get(int(index)) if index is not None else self.focused_node
			text = str(message.get("text", ""))
			if node is not None:
				await self.session.event_bus.dispatch(ClickElementEvent(node=node))
				await self.session.event_bus.dispatch(TypeTextEvent(node=node, text=text))
				self.focused_node = node
			else:
				# Standard computer.type targets the currently focused control. The
				# optional mark id takes the safer browser-use element path above;
				# coordinate clicks still need a literal focused-input fallback.
				cdp = await self.session.get_or_create_cdp_session(target_id=None, focus=False)
				await cdp.cdp_client.send.Input.insertText(
					params={"text": text}, session_id=cdp.session_id
				)
			return {"status": "typed"}
		if command == "key":
			await self.session.event_bus.dispatch(
				SendKeysEvent(keys="+".join(str(key) for key in message.get("keys", [])))
			)
			return {"status": "pressed"}
		if command == "scroll":
			delta_y = int(message.get("delta_y", 0))
			delta_x = int(message.get("delta_x", 0))
			if delta_y:
				await self.session.event_bus.dispatch(
					ScrollEvent(direction="down" if delta_y > 0 else "up", amount=abs(delta_y))
				)
			elif delta_x:
				await self.session.event_bus.dispatch(
					ScrollEvent(direction="right" if delta_x > 0 else "left", amount=abs(delta_x))
				)
			return {"status": "scrolled"}
		if command == "wait":
			await asyncio.sleep(min(10, max(0, int(message.get("duration_ms", 0))) / 1000))
			return {"status": "waited"}
		raise ValueError(f"unknown browser bridge command {command!r}")

	async def mouse_event(
		self,
		kind: str,
		x: int,
		y: int,
		*,
		button: str = "none",
		buttons: int = 0,
		click_count: int = 0,
	) -> None:
		"""Send pointer movement through browser-use's managed CDP session."""
		cdp = await self.session.get_or_create_cdp_session(target_id=None, focus=False)
		await cdp.cdp_client.send.Input.dispatchMouseEvent(
			params={
				"type": kind,
				"x": x,
				"y": y,
				"button": button,
				"buttons": buttons,
				"clickCount": click_count,
			},
			session_id=cdp.session_id,
		)

	async def click_mouse(self, x: int, y: int, *, button: str, count: int) -> None:
		buttons = {"left": 1, "right": 2, "middle": 4}.get(button)
		if buttons is None:
			raise ValueError(f"unsupported mouse button {button!r}")
		for click_count in range(1, count + 1):
			await self.mouse_event(
				"mousePressed", x, y, button=button, buttons=buttons, click_count=click_count
			)
			await self.mouse_event(
				"mouseReleased", x, y, button=button, buttons=0, click_count=click_count
			)

	async def capture(self) -> dict[str, Any]:
		state = await self.session.get_browser_state_summary(include_screenshot=True)
		self.selector_map = dict(state.dom_state.selector_map or {})
		if not state.screenshot:
			raise RuntimeError("browser-use returned no screenshot")
		cdp = await self.session.get_or_create_cdp_session(target_id=None, focus=False)
		highlighted = await create_highlighted_screenshot_async(
			state.screenshot, self.selector_map, cdp_session=cdp, filter_highlight_ids=False
		)
		elements: list[dict[str, Any]] = []
		for index, node in self.selector_map.items():
			rect = node.snapshot_node.clientRects if node.snapshot_node else None
			elements.append(
				{
					"id": str(index),
					"tag": node.node_name.lower(),
					"name": (node.ax_node.name if node.ax_node else None)
					or node.get_all_children_text(max_depth=2)[:160],
					"bounds": (
						{"x": rect.x, "y": rect.y, "width": rect.width, "height": rect.height}
						if rect
						else None
					),
				}
			)
		page = state.page_info
		return {
			"status": "captured",
			"url": state.url,
			"title": state.title,
			"width": page.viewport_width if page else 1280,
			"height": page.viewport_height if page else 720,
			"frame": highlighted,
			"elements": elements,
		}


async def main(cdp_url: str) -> None:
	bridge = Bridge(cdp_url)
	await bridge.start()
	print(json.dumps({"status": "ready"}), flush=True)
	try:
		while line := await asyncio.to_thread(sys.stdin.readline):
			request_id: Any = None
			try:
				message = json.loads(line)
				request_id = message.get("id")
				result = await bridge.dispatch(message)
				result.update({"id": request_id, "ok": True})
			except Exception as error:  # Every command must receive an answer.
				result = {"id": request_id, "ok": False, "error": str(error)}
			print(json.dumps(result, separators=(",", ":")), flush=True)
	finally:
		await bridge.close()


if __name__ == "__main__":
	logging.basicConfig(stream=sys.stderr, level=logging.WARNING)
	parser = argparse.ArgumentParser()
	parser.add_argument("--cdp-url", default="http://127.0.0.1:9222")
	arguments = parser.parse_args()
	asyncio.run(main(arguments.cdp_url))
