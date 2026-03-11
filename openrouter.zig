const std = @import("std");

fn InvokeIntelligence(
    allocator: std.mem.Allocator,
    api_key: []const u8,
    messages: anytype,
) ![]u8 {
    const request = .{
        .model = "nvidia/nemotron-3-super-120b-a12b:free",
        .messages = messages,
        .reasoning = .{ .enabled = false },
    };

    // Build JSON body using Stringify.valueAlloc
    const body = try std.json.Stringify.valueAlloc(allocator, request, .{});
    defer allocator.free(body);

    const auth_header = try std.fmt.allocPrint(allocator, "Bearer {s}", .{api_key});
    defer allocator.free(auth_header);

    var client: std.http.Client = .{ .allocator = allocator };
    defer client.deinit();

    var aw: std.io.Writer.Allocating = .init(allocator);
    defer aw.deinit();

    const result = try client.fetch(.{
        .location = .{ .url = "https://openrouter.ai/api/v1/chat/completions" },
        .method = .POST,
        .headers = .{
            .content_type = .{ .override = "application/json" },
            .authorization = .{ .override = auth_header },
        },
        .payload = body,
        .response_writer = &aw.writer,
    });

    const response_bytes = aw.written();

    if (result.status != .ok) {
        return std.fmt.allocPrint(allocator, "HTTP Error: {}\n{s}", .{ result.status, response_bytes });
    }

    const parsed = std.json.parseFromSlice(std.json.Value, allocator, response_bytes, .{}) catch {
        return try allocator.dupe(u8, response_bytes);
    };
    defer parsed.deinit();

    if (parsed.value.object.get("choices")) |choices_val| {
        if (choices_val.array.items.len > 0) {
            const first_choice = choices_val.array.items[0];
            if (first_choice.object.get("message")) |msg| {
                if (msg.object.get("content")) |content| {
                    if (content == .string) {
                        return try allocator.dupe(u8, content.string);
                    }
                }
            }
        }
    }

    return try allocator.dupe(u8, response_bytes);
}

pub fn main() !void {
    var gpa: std.heap.GeneralPurposeAllocator(.{}) = .{};
    defer _ = gpa.deinit();
    const allocator = gpa.allocator();

    const api_key = std.posix.getenv("OPENROUTER_API_KEY") orelse {
        std.debug.print("OPENROUTER_API_KEY not set\n", .{});
        return;
    };

    const messages = .{
        .{ .role = "system", .content = "You are an assistant." },
        .{ .role = "user", .content = "How many r's are in the word strawberry?" },
    };

    const response = try InvokeIntelligence(allocator, api_key, messages);
    defer allocator.free(response);

    std.debug.print("{s}\n", .{response});
}
