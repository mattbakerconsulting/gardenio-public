// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

/// Parse numbered option labels from Gemini CLI rendered screen lines.
///
/// Gemini wraps permission prompts in a box-drawing border:
/// ```text
/// │ ● 1. Allow once                      │
/// │   2. Allow for this session          │
/// │   3. No, suggest changes (esc)       │
/// ```
///
/// This parser strips box borders (`│`), handles the `●` selection indicator,
/// and extracts `N. label` patterns from bottom-up scanning.
pub fn parse_options_from_screen(lines: &[String]) -> Vec<String> {
    let mut options: Vec<(u32, String)> = Vec::new();
    let mut found_any = false;

    for line in lines.iter().rev() {
        let trimmed = line.trim();

        if trimmed.is_empty() {
            continue;
        }

        // Skip box border lines (╭─...─╮, ╰─...─╯, pure ─)
        if is_box_border(trimmed) {
            if found_any {
                // Top border above the options — stop scanning.
                break;
            }
            continue;
        }

        // Skip spinner/status lines outside the box
        if is_status_line(trimmed) {
            continue;
        }

        // Strip box side borders: │ content │
        let content = strip_box_sides(trimmed);

        // Try to parse as a numbered option
        if let Some((num, label)) = parse_numbered_option(content) {
            options.push((num, label));
            found_any = true;
        } else if found_any {
            // Non-option line inside the box — could be a blank padded line
            // or descriptive text above the options. Blank-padded lines
            // (just box borders with spaces) should be skipped.
            if content.trim().is_empty() {
                continue;
            }
            // Otherwise we've hit content above the options block — stop.
            break;
        }
    }

    options.sort_by_key(|(num, _)| *num);
    options.into_iter().map(|(_, label)| label).collect()
}

/// Try to parse a line as a numbered option: `[● ] N. label`.
///
/// Strips leading selection indicator (`●`) and whitespace before matching.
fn parse_numbered_option(content: &str) -> Option<(u32, String)> {
    let s = content.strip_prefix('●').unwrap_or(content);
    let s = s.trim_start();

    // Must start with one or more digits
    let digit_end = s.find(|c: char| !c.is_ascii_digit())?;
    if digit_end == 0 {
        return None;
    }

    let num: u32 = s[..digit_end].parse().ok()?;

    // Must be followed by ". "
    let rest = s[digit_end..].strip_prefix(". ")?;

    let label = rest.trim_end().to_string();
    if label.is_empty() {
        return None;
    }

    Some((num, label))
}

/// Box border lines start/end with corner or horizontal box-drawing chars.
fn is_box_border(trimmed: &str) -> bool {
    !trimmed.is_empty()
        && trimmed.chars().all(|c| matches!(c, '─' | '╌' | '━' | '═' | '╭' | '╮' | '╰' | '╯'))
}

/// Status/spinner lines outside the box (e.g. "⠏ Waiting for user confirmation...")
fn is_status_line(trimmed: &str) -> bool {
    // Braille spinner characters: U+2800..U+28FF
    trimmed.starts_with(|c: char| ('\u{2800}'..='\u{28FF}').contains(&c))
}

/// Strip leading `│` and trailing `│` from a box-bordered line.
fn strip_box_sides(trimmed: &str) -> &str {
    let s = trimmed.strip_prefix('│').unwrap_or(trimmed);
    let s = s.strip_suffix('│').unwrap_or(s);
    s.trim()
}

#[cfg(test)]
#[path = "screen_tests.rs"]
mod tests;
