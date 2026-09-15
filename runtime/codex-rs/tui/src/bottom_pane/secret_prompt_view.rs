//! Single-line secret input that never stores or renders plaintext outside a
//! zeroizing buffer.

use codexmarathon_transfer::SecretString;
use crossterm::event::KeyCode;
use crossterm::event::KeyEvent;
use crossterm::event::KeyModifiers;
use ratatui::buffer::Buffer;
use ratatui::layout::Rect;
use ratatui::style::Stylize;
use ratatui::text::Line;
use ratatui::text::Span;
use ratatui::widgets::Clear;
use ratatui::widgets::Paragraph;
use ratatui::widgets::Widget;
use unicode_segmentation::UnicodeSegmentation;
use unicode_width::UnicodeWidthStr;
use zeroize::Zeroize;
use zeroize::Zeroizing;

use crate::bottom_pane::CancellationEvent;
use crate::bottom_pane::bottom_pane_view::BottomPaneView;
use crate::bottom_pane::bottom_pane_view::ViewCompletion;
use crate::bottom_pane::popup_consts::standard_popup_hint_line;
use crate::key_hint::has_ctrl_or_alt;
use crate::keymap::KeymapContext;
use crate::keymap::KeymapContextSet;
use crate::render::renderable::Renderable;

pub(crate) type SecretSubmitted = Box<dyn Fn(SecretString) + Send + Sync>;
pub(crate) type SecretCancelled = Box<dyn Fn() + Send + Sync>;

pub(crate) struct SecretPromptView {
    title: String,
    context_label: Option<String>,
    value: Zeroizing<String>,
    on_submit: SecretSubmitted,
    on_cancel: SecretCancelled,
    completion: Option<ViewCompletion>,
}

impl SecretPromptView {
    pub(crate) fn new(
        title: String,
        context_label: Option<String>,
        on_submit: SecretSubmitted,
        on_cancel: SecretCancelled,
    ) -> Self {
        Self {
            title,
            context_label,
            value: Zeroizing::new(String::new()),
            on_submit,
            on_cancel,
            completion: None,
        }
    }

    fn cancel(&mut self) {
        if self.completion.is_none() {
            (self.on_cancel)();
            self.completion = Some(ViewCompletion::Cancelled);
        }
    }

    fn submit(&mut self) {
        if self.completion.is_some() || self.value.is_empty() {
            return;
        }
        let secret = SecretString::from(std::mem::take(&mut *self.value));
        (self.on_submit)(secret);
        self.completion = Some(ViewCompletion::Accepted);
    }

    fn remove_last_grapheme(&mut self) {
        if let Some((index, _)) = self.value.grapheme_indices(true).next_back() {
            // Wrap the destination before copying any secret bytes into it. Building a
            // `String` with `to_string()` first would leave that temporary allocation
            // outside zeroizing ownership.
            let mut retained = Zeroizing::new(String::with_capacity(index));
            retained.push_str(&self.value[..index]);
            self.value.zeroize();
            std::mem::swap(&mut self.value, &mut retained);
        }
    }

    fn masked_value(&self) -> String {
        "•".repeat(self.value.graphemes(true).count())
    }
}

impl BottomPaneView for SecretPromptView {
    fn keymap_contexts(&self) -> KeymapContextSet {
        KeymapContextSet::new(KeymapContext::Editor)
    }

    fn handle_key_event(&mut self, event: KeyEvent) {
        match event {
            KeyEvent {
                code: KeyCode::Enter,
                modifiers: KeyModifiers::NONE,
                ..
            } => self.submit(),
            KeyEvent {
                code: KeyCode::Esc, ..
            } => self.cancel(),
            KeyEvent {
                code: KeyCode::Backspace,
                ..
            } => self.remove_last_grapheme(),
            KeyEvent {
                code: KeyCode::Char(character),
                modifiers,
                ..
            } if !has_ctrl_or_alt(modifiers) => self.value.push(character),
            _ => {}
        }
    }

    fn on_ctrl_c(&mut self) -> CancellationEvent {
        self.cancel();
        CancellationEvent::Handled
    }

    fn is_complete(&self) -> bool {
        self.completion.is_some()
    }

    fn completion(&self) -> Option<ViewCompletion> {
        self.completion
    }

    fn handle_paste(&mut self, pasted: String) -> bool {
        if pasted.is_empty() {
            return false;
        }
        let pasted = Zeroizing::new(pasted);
        self.value.push_str(&pasted);
        true
    }

    fn is_secret_input(&self) -> bool {
        true
    }
}

impl Renderable for SecretPromptView {
    fn desired_height(&self, _width: u16) -> u16 {
        if self.context_label.is_some() { 5 } else { 4 }
    }

    fn render(&self, area: Rect, buf: &mut Buffer) {
        if area.is_empty() {
            return;
        }
        Clear.render(area, buf);
        Paragraph::new(Line::from(vec![gutter(), self.title.clone().bold()]))
            .render(Rect::new(area.x, area.y, area.width, 1), buf);
        let mut input_y = area.y.saturating_add(1);
        if let Some(context) = &self.context_label {
            Paragraph::new(Line::from(vec![gutter(), context.clone().cyan()]))
                .render(Rect::new(area.x, input_y, area.width, 1), buf);
            input_y = input_y.saturating_add(1);
        }
        let masked = self.masked_value();
        let value = if masked.is_empty() {
            Line::from("Password is hidden".dim())
        } else {
            Line::from(masked)
        };
        Paragraph::new(Line::from(vec![gutter()]))
            .render(Rect::new(area.x, input_y, area.width.min(2), 1), buf);
        Paragraph::new(value).render(
            Rect::new(
                area.x.saturating_add(2),
                input_y,
                area.width.saturating_sub(2),
                1,
            ),
            buf,
        );
        let hint_y = input_y.saturating_add(2);
        if hint_y < area.bottom() {
            Paragraph::new(standard_popup_hint_line()).render(
                Rect::new(
                    area.x.saturating_add(2),
                    hint_y,
                    area.width.saturating_sub(2),
                    1,
                ),
                buf,
            );
        }
    }

    fn cursor_pos(&self, area: Rect) -> Option<(u16, u16)> {
        let input_y = area
            .y
            .saturating_add(1)
            .saturating_add(u16::from(self.context_label.is_some()));
        let width = UnicodeWidthStr::width(self.masked_value().as_str()) as u16;
        Some((
            area.x
                .saturating_add(2)
                .saturating_add(width.min(area.width.saturating_sub(3))),
            input_y,
        ))
    }

    fn cursor_style(&self, _area: Rect) -> crossterm::cursor::SetCursorStyle {
        crossterm::cursor::SetCursorStyle::SteadyBar
    }
}

fn gutter() -> Span<'static> {
    "▌ ".cyan()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use std::sync::Mutex;

    #[test]
    fn render_never_contains_secret_plaintext() {
        let submitted = Arc::new(Mutex::new(false));
        let callback_state = Arc::clone(&submitted);
        let mut prompt = SecretPromptView::new(
            "Password".to_string(),
            None,
            Box::new(move |_secret| *callback_state.lock().expect("lock") = true),
            Box::new(|| {}),
        );
        assert!(prompt.handle_paste("sentinel-password".to_string()));
        let area = Rect::new(0, 0, 40, prompt.desired_height(40));
        let mut buffer = Buffer::empty(area);
        prompt.render(area, &mut buffer);
        let rendered = buffer
            .content
            .iter()
            .map(|cell| cell.symbol())
            .collect::<String>();
        assert!(!rendered.contains("sentinel-password"));
        assert!(rendered.contains('•'));
        prompt.handle_key_event(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE));
        assert!(*submitted.lock().expect("lock"));
    }
}
