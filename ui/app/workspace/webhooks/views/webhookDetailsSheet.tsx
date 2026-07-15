import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdownMenu";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useCopyToClipboard } from "@/hooks/useCopyToClipboard";
import { getErrorMessage, useGetWebhookDeliveriesQuery, useRedeliverWebhookDeliveryMutation } from "@/lib/store";
import {
	WEBHOOK_EVENT_COLORS,
	WEBHOOK_TUNING_DEFAULTS,
	WebhookDelivery,
	WebhookDeliveryOutcome,
	WebhookEndpoint,
	WebhookEvent,
} from "@/lib/types/webhooks";
import { format, formatDistanceToNow } from "date-fns";
import { ChevronDown, ChevronLeft, ChevronRight, Loader2, RefreshCcw, Send } from "lucide-react";
import { Fragment, useEffect, useMemo, useState } from "react";
import { toast } from "sonner";

const PAGE_SIZE = 25;

const OUTCOME_COLORS: Record<WebhookDeliveryOutcome, string> = {
	delivered: "bg-green-100 text-green-800",
	retryable_failure: "bg-yellow-100 text-yellow-800",
	permanent_failure: "bg-red-100 text-red-800",
	exhausted: "bg-red-100 text-red-800",
};

const OUTCOME_LABELS: Record<WebhookDeliveryOutcome, string> = {
	delivered: "delivered",
	retryable_failure: "retrying",
	permanent_failure: "failed",
	exhausted: "retries exhausted",
};

const DetailEntry = ({ label, value }: { label: string; value: React.ReactNode }) => (
	<div>
		<div className="text-muted-foreground text-xs">{label}</div>
		<div className="text-sm font-medium break-all">{value}</div>
	</div>
);

const relativeTime = (timestamp?: string) => (timestamp ? formatDistanceToNow(new Date(timestamp), { addSuffix: true }) : "never");

// Wraps a badge with the attempt's error text as a tooltip when present.
const withErrorTooltip = (badge: React.ReactNode, error?: string) => {
	if (!error) {
		return badge;
	}
	return (
		<Tooltip>
			<TooltipTrigger>{badge}</TooltipTrigger>
			{/* text-wrap overrides the component's text-balance, which leaves a
			    right-side gap by shortening lines inside a full-width box. */}
			<TooltipContent className="max-w-[400px] text-wrap break-words">{error}</TooltipContent>
		</Tooltip>
	);
};

// Run-level outcome: where the delivery as a whole stands.
const outcomeBadge = (attempt: WebhookDelivery) =>
	withErrorTooltip(
		<Badge variant="outline" className={OUTCOME_COLORS[attempt.outcome]}>
			{OUTCOME_LABELS[attempt.outcome]}
		</Badge>,
		attempt.error,
	);

// Attempt-level outcome: a single attempt either reached the receiver with a
// 2xx or it didn't — retry scheduling is the run's concern, not the attempt's.
const attemptBadge = (attempt: WebhookDelivery) =>
	withErrorTooltip(
		attempt.outcome === "delivered" ? (
			<Badge variant="outline" className="bg-green-100 text-green-800">
				success
			</Badge>
		) : (
			<Badge variant="outline" className="bg-red-100 text-red-800">
				error
			</Badge>
		),
		attempt.error,
	);

interface WebhookDetailsSheetProps {
	endpoint: WebhookEndpoint | null;
	// Test fires are owned by the parent so the cooldown countdown stays
	// shared with the table's actions menu.
	isTesting: boolean;
	testCooldown: number;
	onTest: (endpoint: WebhookEndpoint, event: WebhookEvent) => void;
	onClose: () => void;
}

export function WebhookDetailsSheet({ endpoint, isTesting, testCooldown, onTest, onClose }: WebhookDetailsSheetProps) {
	const open = !!endpoint;
	const [offset, setOffset] = useState(0);
	const [redeliverWebhookDelivery] = useRedeliverWebhookDeliveryMutation();
	const [redeliveringIds, setRedeliveringIds] = useState<Set<string>>(new Set());
	const { copy } = useCopyToClipboard();

	useEffect(() => {
		setOffset(0);
	}, [endpoint?.id]);

	const { data, isLoading } = useGetWebhookDeliveriesQuery(
		{ endpointId: endpoint?.id ?? "", limit: PAGE_SIZE, offset },
		{ skip: !open, pollingInterval: 5000 },
	);
	const totalCount = data?.pagination.total_count ?? 0;

	// One row per delivery RUN. A redelivery reuses the original webhook_id
	// (receiver-side dedupe) and restarts attempt numbering at 1, so a
	// webhook_id can hold several runs — splitting on the attempt-number
	// restart keeps each row's count within the endpoint's retry budget.
	// Attempts arrive newest-first; runs keep that order via first appearance.
	const runs = useMemo(() => {
		const deliveries = data?.deliveries ?? [];
		const out: { key: string; webhookId: string; runNo: number; attempts: WebhookDelivery[] }[] = [];
		const openRun = new Map<string, WebhookDelivery[]>();
		const runCount = new Map<string, number>();
		for (const delivery of deliveries) {
			const current = openRun.get(delivery.webhook_id);
			if (current && delivery.attempt_no < current[current.length - 1].attempt_no) {
				current.push(delivery);
				continue;
			}
			const run = [delivery];
			openRun.set(delivery.webhook_id, run);
			const runNo = (runCount.get(delivery.webhook_id) ?? 0) + 1;
			runCount.set(delivery.webhook_id, runNo);
			out.push({ key: `${delivery.webhook_id}:${runNo}`, webhookId: delivery.webhook_id, runNo, attempts: run });
		}
		// The oldest run of a webhook_id on this page is the original send;
		// every newer run is a redelivery.
		return out.map((run) => ({ ...run, isRedelivery: run.runNo < (runCount.get(run.webhookId) ?? 1) }));
	}, [data]);

	const [expandedIds, setExpandedIds] = useState<Set<string>>(new Set());
	const toggleExpanded = (webhookId: string) => {
		setExpandedIds((prev) => {
			const next = new Set(prev);
			if (next.has(webhookId)) {
				next.delete(webhookId);
			} else {
				next.add(webhookId);
			}
			return next;
		});
	};

	const handleRedeliver = async (deliveryId: string) => {
		setRedeliveringIds((prev) => new Set(prev).add(deliveryId));
		try {
			await redeliverWebhookDelivery(deliveryId).unwrap();
			toast.success("Redelivery queued under the original webhook id");
		} catch (err) {
			toast.error(getErrorMessage(err));
		} finally {
			setRedeliveringIds((prev) => {
				const next = new Set(prev);
				next.delete(deliveryId);
				return next;
			});
		}
	};

	// Effective knob value with its unit; unset knobs show the worker default.
	const tuning = (key: keyof typeof WEBHOOK_TUNING_DEFAULTS, unit = "") => {
		const value = endpoint?.[key] || WEBHOOK_TUNING_DEFAULTS[key];
		return `${value}${unit}`;
	};

	return (
		<Sheet open={open} onOpenChange={(sheetOpen) => !sheetOpen && onClose()}>
			<SheetContent className="flex w-full flex-col gap-0 overflow-x-hidden p-8 sm:max-w-[50%]">
				<SheetHeader className="flex flex-col items-start px-0">
					<SheetTitle className="flex w-fit items-center gap-2 font-medium">
						<p className="text-md max-w-full truncate">{endpoint?.name}</p>
						{endpoint?.disabled ? (
							<Badge variant="outline" className="bg-gray-100 text-gray-800">
								disabled
							</Badge>
						) : (
							<Badge variant="outline" className="bg-green-100 text-green-800">
								enabled
							</Badge>
						)}
					</SheetTitle>
					<SheetDescription className="break-all">{endpoint?.url}</SheetDescription>
				</SheetHeader>

				<div className="space-y-4 rounded-sm border p-4">
					<div className="grid grid-cols-3 gap-4">
						<DetailEntry
							label="Events"
							value={
								<div className="flex flex-wrap gap-1">
									{endpoint?.events.map((event) => (
										<Badge key={event} variant="outline" className={`font-mono text-xs ${WEBHOOK_EVENT_COLORS[event]}`}>
											{event}
										</Badge>
									))}
								</div>
							}
						/>
						<DetailEntry label="Include response" value={endpoint?.include_response ? "yes" : "no"} />
						<DetailEntry label="Private network" value={endpoint?.allow_private_network ? "allowed" : "blocked"} />
						<DetailEntry label="Last success" value={relativeTime(endpoint?.last_success_at)} />
						<DetailEntry label="Last failure" value={relativeTime(endpoint?.last_failure_at)} />
						<DetailEntry label="Consecutive failures" value={endpoint?.consecutive_failures ?? 0} />
						<DetailEntry label="Max retries" value={tuning("max_retries")} />
						<DetailEntry
							label="Retry backoff"
							value={`${tuning("retry_backoff_initial_seconds", "s")} → ${tuning("retry_backoff_max_seconds", "s")}`}
						/>
						<DetailEntry label="Attempt timeout" value={tuning("attempt_timeout_seconds", "s")} />
					</div>
				</div>

				<div className="mt-4 flex items-center justify-between">
					<h3 className="font-semibold">Delivery History</h3>
					<DropdownMenu>
						<DropdownMenuTrigger asChild>
							<Button
								variant="outline"
								size="sm"
								disabled={isTesting || testCooldown > 0 || endpoint?.disabled}
								data-testid="webhook-test-fire-btn"
							>
								{isTesting ? <Loader2 className="h-4 w-4 animate-spin" /> : <Send className="h-4 w-4" />}
								{testCooldown > 0 ? `Retry in ${testCooldown}s` : "Send Test Event"}
								<ChevronDown className="h-3 w-3" />
							</Button>
						</DropdownMenuTrigger>
						<DropdownMenuContent align="end">
							{endpoint?.events.map((event) => (
								<DropdownMenuItem
									key={event}
									className="cursor-pointer"
									data-testid={`webhook-test-fire-${event}`}
									onSelect={() => onTest(endpoint, event)}
								>
									{event}
								</DropdownMenuItem>
							))}
						</DropdownMenuContent>
					</DropdownMenu>
				</div>

				<div className="mt-4 min-h-0 flex-1 overflow-auto rounded-sm border">
					<Table>
						<TableHeader className="bg-muted sticky top-0 z-10">
							<TableRow>
								<TableHead className="w-8"></TableHead>
								<TableHead>Time</TableHead>
								<TableHead>Request ID</TableHead>
								<TableHead>Event</TableHead>
								<TableHead>Attempts</TableHead>
								<TableHead>Outcome</TableHead>
								<TableHead>Status Code</TableHead>
								<TableHead className="text-right">Actions</TableHead>
							</TableRow>
						</TableHeader>
						<TableBody>
							{isLoading ? (
								<TableRow>
									<TableCell colSpan={8} className="h-24 text-center">
										<Loader2 className="mx-auto h-4 w-4 animate-spin" />
									</TableCell>
								</TableRow>
							) : runs.length === 0 ? (
								<TableRow>
									<TableCell colSpan={8} className="text-muted-foreground h-24 text-center">
										No deliveries yet.
									</TableCell>
								</TableRow>
							) : (
								runs.map((run) => {
									const latest = run.attempts[0];
									const expanded = expandedIds.has(run.key);
									return (
										<Fragment key={run.key}>
											<TableRow
												className="cursor-pointer"
												onClick={() => toggleExpanded(run.key)}
												data-testid={`webhook-delivery-row-${run.key}`}
											>
												<TableCell>
													{run.attempts.length > 1 &&
														(expanded ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />)}
												</TableCell>
												<TableCell className="whitespace-nowrap">{relativeTime(latest.created_at)}</TableCell>
												<TableCell onClick={(e) => e.stopPropagation()}>
													<Tooltip>
														<TooltipTrigger asChild>
															<code
																className="cursor-pointer font-mono text-xs"
																onClick={() => copy(latest.async_job_id)}
																data-testid={`webhook-delivery-request-id-${run.key}`}
															>
																{latest.async_job_id.slice(0, 8)}…
															</code>
														</TooltipTrigger>
														<TooltipContent className="font-mono">{latest.async_job_id}</TooltipContent>
													</Tooltip>
												</TableCell>
												<TableCell className="whitespace-nowrap">
													<div className="flex items-center gap-1">
														<Badge variant="outline" className={`font-mono text-xs ${WEBHOOK_EVENT_COLORS[latest.event]}`}>
															{latest.event}
														</Badge>
														{run.isRedelivery && (
															<Badge variant="outline" className="text-muted-foreground text-xs">
																redelivery
															</Badge>
														)}
													</div>
												</TableCell>
												<TableCell>{run.attempts.length}</TableCell>
												<TableCell>{outcomeBadge(latest)}</TableCell>
												<TableCell>{latest.status_code || "-"}</TableCell>
												<TableCell className="text-right" onClick={(e) => e.stopPropagation()}>
													<Button
														variant="ghost"
														size="sm"
														onClick={() => handleRedeliver(latest.id)}
														disabled={redeliveringIds.has(latest.id) || endpoint?.disabled}
														data-testid={`webhook-redeliver-btn-${run.key}`}
														aria-label="Redeliver"
													>
														{redeliveringIds.has(latest.id) ? (
															<Loader2 className="h-4 w-4 animate-spin" />
														) : (
															<RefreshCcw className="h-4 w-4" />
														)}
													</Button>
												</TableCell>
											</TableRow>
											{expanded &&
												run.attempts.map((attempt) => (
													<TableRow key={attempt.id} className="bg-muted/30">
														<TableCell></TableCell>
														<TableCell className="text-muted-foreground whitespace-nowrap">
															{format(new Date(attempt.created_at), "MMM d, yyyy hh:mm:ss aa")}
														</TableCell>
														<TableCell className="text-muted-foreground" colSpan={3}>
															Attempt {attempt.attempt_no}
														</TableCell>
														<TableCell>{attemptBadge(attempt)}</TableCell>
														<TableCell>{attempt.status_code || "-"}</TableCell>
														<TableCell></TableCell>
													</TableRow>
												))}
										</Fragment>
									);
								})
							)}
						</TableBody>
					</Table>
				</div>

				{totalCount > 0 && (
					<div className="flex shrink-0 items-center justify-between text-xs" data-testid="pagination">
						<div className="text-muted-foreground flex items-center gap-2">
							{(offset + 1).toLocaleString()}-{Math.min(offset + PAGE_SIZE, totalCount).toLocaleString()} of {totalCount.toLocaleString()}{" "}
							entries
						</div>
						<div className="flex items-center gap-2">
							<Button
								variant="ghost"
								size="sm"
								onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
								disabled={offset === 0}
								aria-label="Previous page"
							>
								<ChevronLeft className="size-3" />
							</Button>
							<div className="flex items-center gap-1">
								<span>Page</span>
								<span>{Math.floor(offset / PAGE_SIZE) + 1}</span>
								<span>of {Math.ceil(totalCount / PAGE_SIZE)}</span>
							</div>
							<Button
								variant="ghost"
								size="sm"
								onClick={() => setOffset(offset + PAGE_SIZE)}
								disabled={offset + PAGE_SIZE >= totalCount}
								aria-label="Next page"
							>
								<ChevronRight className="size-3" />
							</Button>
						</div>
					</div>
				)}
			</SheetContent>
		</Sheet>
	);
}